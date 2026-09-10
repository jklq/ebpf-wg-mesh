package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"errors"
	"github.com/google/uuid"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/source"
)

var ErrBuildCommitMismatch = errors.New("commit_sha does not match the claimed build")

type BuildCompletion struct {
	Build            BuildRunRecord
	Changed          bool
	RolloutScheduled bool
}

func (d *Delivery) CompleteBuild(ctx context.Context, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) (BuildCompletion, error) {
	s := d.store
	var serviceID string
	var changed, rolloutScheduled bool
	var completed BuildRunRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed, rolloutScheduled = false, false
		completed = BuildRunRecord{}
		build, err := s.buildRunByIDQuerier(ctx, tx, buildID)
		if err != nil {
			return err
		}
		serviceID = build.ServiceID
		if err := s.lockServiceTx(ctx, tx, build.ServiceID); err != nil {
			return err
		}
		build, err = s.buildRunByIDQuerier(ctx, tx, buildID)
		if err != nil {
			return err
		}
		if build.BuilderID != builderID {
			return ErrBuildNotOwned
		}
		if BuildStateTerminal(build.State) {
			return nil
		}
		if build.State != BuildStateRunning {
			return ErrBuildNotOwned
		}
		if commitSHA != build.CommitSHA {
			return ErrBuildCommitMismatch
		}
		completed = build
		dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, build.ServiceID, build.ID)
		if err != nil {
			return err
		}
		if ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3 WHERE id = $4 AND state = $5`,
				BuildStateCancelled, "cancelled; late builder completion ignored", now, buildID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE builder_workers SET current_build_id = '', last_heartbeat_at = $1, updated_at = $1 WHERE id = $2`,
				now, builderID,
			); err != nil {
				return err
			}
			changed = true
			completed.State = BuildStateCancelled
			return nil
		}
		now := time.Now().UTC()
		stateValue := BuildStateFailed
		switch state {
		case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
			stateValue = BuildStateSucceeded
		case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
			stateValue = BuildStateSuperseded
		case platformv1.BuildState_BUILD_STATE_FAILED:
			stateValue = BuildStateFailed
		default:
			return errors.New("invalid terminal build state")
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        commit_sha = $2,
			        image_digest = $3,
			        failure_reason = $4,
			        finished_at = $5,
			        builder_id = $6
			  WHERE id = $7 AND state = $8`,
			stateValue, commitSHA, imageDigest, failureReason, now, builderID, buildID, BuildStateRunning,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return nil
		}
		changed = true
		completed.State = stateValue
		completed.ImageDigest = imageDigest
		completed.FailureReason = failureReason
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers
			    SET current_build_id = '',
			        last_heartbeat_at = $1,
			        updated_at = $1
			  WHERE id = $2`,
			now, builderID,
		); err != nil {
			return err
		}

		if stateValue != BuildStateSucceeded {
			toState := DeploymentStateFailed
			reasonCode := reasonBuildFailed
			detail := FirstNonEmpty(failureReason, "Build failed")
			if stateValue == BuildStateSuperseded {
				toState = DeploymentStateSuperseded
				reasonCode = reasonBuildSuperseded
				detail = FirstNonEmpty(failureReason, "Build superseded")
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:          toState,
				Actor:            deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
				ReasonCode:       reasonCode,
				Detail:           detail,
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return nil
		}
		if build.SourceRevisionID == "" || build.SourceSnapshotID == "" || build.SourceSnapshotDigest == "" {
			return errSourceStateNotReady
		}
		var newerCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*)
			   FROM build_runs
			  WHERE service_id = $1
			    AND queued_at > $2
			    AND state IN ($3, $4, $5)`,
			build.ServiceID, build.QueuedAt, BuildStateQueued, BuildStateRunning, BuildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		if newerCount > 0 {
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:          DeploymentStateSuperseded,
				Actor:            deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
				ReasonCode:       reasonBuildSuperseded,
				Detail:           "A newer build superseded this image",
				ImageDigest:      imageDigest,
				HasImageDigest:   true,
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return nil
		}

		currentDep, currentOK, err := s.currentDeploymentTx(ctx, tx, build.ServiceID)
		if err != nil {
			return err
		}
		if currentOK && (deploymentStateTerminal(currentDep.State) || currentDep.BuildID != build.ID) {
			return nil
		}
		service, err := s.serviceByIDInternalQuerier(ctx, tx, build.ServiceID)
		if err != nil {
			return err
		}
		nextRolloutGeneration := service.RolloutGeneration
		var currentRolloutState string
		var currentRolloutSpec int64
		err = tx.QueryRowContext(ctx,
			`SELECT state, spec_revision FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
			build.ServiceID, service.RolloutGeneration,
		).Scan(&currentRolloutState, &currentRolloutSpec)
		usePendingRollout := err == nil && currentRolloutState == rolloutStatePendingBuild && currentRolloutSpec == service.SpecRevision
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if !usePendingRollout && ServiceVolumeName(service.Spec) != "" {
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:        DeploymentStateFailed,
				Actor:          deploymentActor{Kind: DeploymentCauseSystem},
				ReasonCode:     reasonDeploymentFailed,
				Detail:         ErrVolumeRollingUnsupported.Error(),
				ImageDigest:    imageDigest,
				HasImageDigest: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return nil
		}
		if !usePendingRollout && (currentRolloutState == rolloutStateInProgress || currentRolloutState == rolloutStatePendingBuild) {
			existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, build.ServiceID, true)
			if err != nil {
				return err
			}
			if _, err := d.prepareReplacementRolloutTx(ctx, tx, service, existing, now); err != nil {
				return err
			}
		}
		if !usePendingRollout {
			nextRolloutGeneration++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET current_resolved_image = $1,
			        last_successful_commit_sha = $2,
			        current_rollout_generation = $3,
			        latest_build_id = $4,
			        updated_at = $5
			  WHERE id = $6`,
			imageDigest, commitSHA, nextRolloutGeneration, buildID, now, build.ServiceID,
		); err != nil {
			return err
		}
		journal.RecordService(ctx, build.ServiceID)
		if usePendingRollout {
			if _, err := tx.ExecContext(ctx,
				`UPDATE service_rollouts
				    SET state = $1, image_digest = $2, build_id = $3
				  WHERE service_id = $4 AND rollout_generation = $5`,
				rolloutStateInProgress, imageDigest, build.ID, build.ServiceID, nextRolloutGeneration,
			); err != nil {
				return err
			}
			journal.RecordRollout(ctx, build.ServiceID, nextRolloutGeneration)
		} else if err := s.insertServiceRolloutTx(ctx, tx, build.ServiceID, nextRolloutGeneration, service.SpecRevision, "build-success", build.ID, "", now); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
			ToState:           DeploymentStateScheduling,
			Actor:             deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
			ReasonCode:        reasonBuildSucceeded,
			Detail:            "Image ready; scheduling rollout",
			ImageDigest:       imageDigest,
			HasImageDigest:    true,
			RolloutGeneration: nextRolloutGeneration,
			HasRollout:        true,
			SpecRevision:      service.SpecRevision,
			HasSpecRevision:   true,
			IgnoreIfTerminal:  true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := d.advanceRolloutTx(ctx, tx, build.ServiceID, now); err != nil {
			return err
		}
		rolloutScheduled = true
		return nil
	})
	if err != nil || !changed {
		return BuildCompletion{}, err
	}
	if rolloutScheduled && d.notifier != nil {
		allocations, err := s.listAllocationsByServiceID(ctx, serviceID)
		if err != nil {
			return BuildCompletion{}, err
		}
		seen := make(map[string]struct{}, len(allocations))
		for _, allocation := range allocations {
			if allocation.AgentID == "" {
				continue
			}
			if _, ok := seen[allocation.AgentID]; ok {
				continue
			}
			seen[allocation.AgentID] = struct{}{}
			d.notifier.Notify(allocation.AgentID)
		}
	}
	return BuildCompletion{Build: completed, Changed: changed, RolloutScheduled: rolloutScheduled}, nil
}

func scanBuildRunRow(scanner interface{ Scan(...any) error }) (BuildRunRecord, error) {
	var (
		rec        BuildRunRecord
		recipeJSON []byte
	)
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.CommitSHA,
		&rec.CommitMessage,
		&rec.CommitAuthor,
		&rec.State,
		&rec.BuilderID,
		&rec.ImageDigest,
		&rec.FailureReason,
		&rec.SourceRevisionID,
		&rec.SourceSnapshotID,
		&rec.SourceSnapshotDigest,
		&rec.TargetRolloutGeneration,
		&recipeJSON,
		&rec.QueuedAt,
		&rec.StartedAt,
		&rec.FinishedAt,
	); err != nil {
		return BuildRunRecord{}, err
	}
	var err error
	rec.BuildRecipe, err = source.UnmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) enqueueBuildFromSourceStateTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, revision source.SourceRevisionRecord, snapshot source.SourceSnapshotRecord, buildRecipe *platformv1.BuildRecipe, actor deploymentActor) (BuildRunRecord, error) {
	s := d.store
	if err := s.lockServiceTx(ctx, tx, service.ID); err != nil {
		return BuildRunRecord{}, err
	}
	service, err := s.serviceByIDInternalQuerier(ctx, tx, service.ID)
	if err != nil {
		return BuildRunRecord{}, err
	}
	if revision.ID == "" || snapshot.ID == "" || !sourceSnapshotMatchesRevision(snapshot, revision) {
		return BuildRunRecord{}, errSourceStateNotReady
	}
	if err := source.EnsureReadySnapshot(snapshot); err != nil {
		return BuildRunRecord{}, err
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $2,
		        finished_at = $3,
		        failure_reason = $4
		  WHERE service_id = $1
		    AND state = $5`,
		service.ID, BuildStateSuperseded, now, "superseded by newer queued build", BuildStateQueued,
	); err != nil {
		return BuildRunRecord{}, err
	}

	targetGeneration := service.RolloutGeneration + 1
	var currentRolloutState string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&currentRolloutState); err != nil && err != sql.ErrNoRows {
		return BuildRunRecord{}, err
	}
	if currentRolloutState == rolloutStatePendingBuild {
		targetGeneration = service.RolloutGeneration
	}
	rec := BuildRunRecord{
		ID:                      uuid.NewString(),
		ServiceID:               service.ID,
		ProjectID:               service.ProjectID,
		EnvironmentID:           service.EnvironmentID,
		CommitSHA:               revision.CommitSHA,
		CommitMessage:           revision.CommitMessage,
		CommitAuthor:            revision.CommitAuthor,
		State:                   BuildStateQueued,
		SourceRevisionID:        revision.ID,
		SourceSnapshotID:        snapshot.ID,
		SourceSnapshotDigest:    snapshot.Digest,
		TargetRolloutGeneration: targetGeneration,
		BuildRecipe:             source.CloneBuildRecipe(buildRecipe),
		QueuedAt:                now,
	}
	recipeJSON, err := source.MarshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return BuildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_runs(
			id, service_id, commit_sha, commit_message, commit_author, state,
			source_revision_id, source_snapshot_id, source_snapshot_digest, target_rollout_generation, build_recipe_json,
			builder_id, queued_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, '', $12)`,
		rec.ID, rec.ServiceID, rec.CommitSHA, rec.CommitMessage, rec.CommitAuthor, rec.State,
		rec.SourceRevisionID, rec.SourceSnapshotID, rec.SourceSnapshotDigest, rec.TargetRolloutGeneration, recipeJSON, rec.QueuedAt,
	); err != nil {
		return BuildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE id = $3`,
		rec.ID, now, service.ID,
	); err != nil {
		return BuildRunRecord{}, err
	}
	journal.RecordService(ctx, service.ID)
	if actor.Kind == "" {
		actor.Kind = DeploymentCauseSystem
	}
	reasonCode := reasonBuildQueued
	detail := "Build queued"
	if actor.Kind == DeploymentCauseWebhook {
		reasonCode = reasonWebhookPush
		detail = "Build queued from webhook"
	}
	if _, err := s.insertDeploymentTx(ctx, tx, service.ID, DeploymentStateQueuedBuild, actor, reasonCode, detail, service.SpecRevision, rec.TargetRolloutGeneration, rec.ID, "", "", now); err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) ClaimNextBuild(ctx context.Context, builderID, builderName string, staleAfter time.Duration) (BuildRunRecord, error) {
	s := d.store
	var rec BuildRunRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec = BuildRunRecord{}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO builder_workers(id, name, current_build_id, last_heartbeat_at, created_at, updated_at)
			 VALUES ($1, $2, '', $3, $3, $3)
			 ON CONFLICT(id) DO UPDATE
			    SET name = excluded.name,
			        last_heartbeat_at = excluded.last_heartbeat_at,
			        updated_at = excluded.updated_at`,
			builderID, builderName, now,
		); err != nil {
			return err
		}
		if staleAfter > 0 {
			if err := d.recoverExpiredBuildsTx(ctx, tx, now.Add(-staleAfter)); err != nil {
				return err
			}
		}
		existing, err := scanBuildRunRow(tx.QueryRowContext(ctx, `SELECT `+buildRunSelectColumns+` FROM build_runs WHERE state = $1 AND builder_id = $2 ORDER BY started_at, id LIMIT 1`, BuildStateRunning, builderID))
		if err == nil {
			rec, err = s.buildRunByIDQuerier(ctx, tx, existing.ID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row := tx.QueryRowContext(ctx,
			`SELECT `+buildRunSelectColumns+`
			   FROM build_runs
			  WHERE state = $1
			  ORDER BY queued_at ASC, id ASC
			  LIMIT 1`,
			BuildStateQueued,
		)
		rec, err = scanBuildRunRow(row)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT e.project_id, s.environment_id FROM services s
			JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`, rec.ServiceID).
			Scan(&rec.ProjectID, &rec.EnvironmentID); err != nil {
			return err
		}
		rec.State = BuildStateRunning
		rec.BuilderID = builderID
		rec.StartedAt = sql.NullTime{Time: now, Valid: true}
		if err := s.lockServiceTx(ctx, tx, rec.ServiceID); err != nil {
			return err
		}
		build, err := s.buildRunByIDQuerier(ctx, tx, rec.ID)
		if err != nil {
			return err
		}
		if build.State != BuildStateQueued {
			rec = BuildRunRecord{}
			return nil
		}
		dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, rec.ServiceID, rec.ID)
		if err != nil {
			return err
		}
		if ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
			rec = BuildRunRecord{}
			return nil
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = $2,
			        builder_id = $3
			  WHERE id = $4 AND state = $5`,
			BuildStateRunning, now, builderID, rec.ID, BuildStateQueued,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			rec = BuildRunRecord{}
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers
			    SET current_build_id = $1,
			        last_heartbeat_at = $2,
			        updated_at = $2
			  WHERE id = $3`,
			rec.ID, now, builderID,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState:          DeploymentStateBuilding,
			Actor:            deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
			ReasonCode:       reasonBuildStarted,
			Detail:           "Builder claimed the build",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) RecoverExpiredBuilds(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return d.store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return d.recoverExpiredBuildsTx(ctx, tx, now.Add(-staleAfter))
	})
}

func (d *Delivery) recoverExpiredBuildsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT b.id, b.service_id, b.queued_at
		   FROM build_runs b
		   JOIN builder_workers w ON w.id = b.builder_id
		  WHERE b.state = $1
		    AND w.last_heartbeat_at < $2
		  ORDER BY b.queued_at ASC, b.id ASC`,
		BuildStateRunning, cutoff,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	type expiredBuild struct {
		ID        string
		ServiceID string
		QueuedAt  time.Time
	}
	var expired []expiredBuild
	for rows.Next() {
		var rec expiredBuild
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.QueuedAt); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now, err := dbtx.DatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	for _, rec := range expired {
		var newerCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*)
			   FROM build_runs
			  WHERE service_id = $1
			    AND queued_at > $2
			    AND state IN ($3, $4, $5)`,
			rec.ServiceID, rec.QueuedAt, BuildStateQueued, BuildStateRunning, BuildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		nextState := BuildStateQueued
		failureReason := ""
		startedAt := any(nil)
		if newerCount > 0 {
			nextState = BuildStateSuperseded
			failureReason = "superseded after stale builder heartbeat"
			startedAt = nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = $2,
			        finished_at = CASE WHEN $1 = $3 THEN $4 ELSE NULL END,
			        builder_id = '',
			        failure_reason = $5
			  WHERE id = $6`,
			nextState, startedAt, BuildStateSuperseded, now, failureReason, rec.ID,
		); err != nil {
			return err
		}
		transition := deploymentTransitionInput{
			ToState:          DeploymentStateQueuedBuild,
			Actor:            deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode:       reasonBuildRequeued,
			Detail:           "Builder heartbeat expired; build requeued",
			IgnoreIfTerminal: true,
		}
		if nextState == BuildStateSuperseded {
			transition.ToState = DeploymentStateSuperseded
			transition.ReasonCode = reasonBuildSuperseded
			transition.Detail = failureReason
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, transition); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = '',
		        updated_at = $1
		  WHERE last_heartbeat_at < $2`,
		now, cutoff,
	); err != nil {
		return err
	}
	return nil
}
