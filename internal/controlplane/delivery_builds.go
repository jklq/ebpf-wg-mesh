package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

var errBuildCommitMismatch = errors.New("commit_sha does not match the claimed build")

type buildCompletion struct {
	Build            buildRunRecord
	Changed          bool
	RolloutScheduled bool
}

func (d *Delivery) completeBuild(ctx context.Context, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) (buildCompletion, error) {
	s := d.store
	var environmentID, serviceID string
	var changed, rolloutScheduled bool
	var completed buildRunRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		changed, rolloutScheduled = false, false
		completed = buildRunRecord{}
		build, err := s.buildRunByIDQuerier(ctx, tx, buildID)
		if err != nil {
			return err
		}
		environmentID = build.EnvironmentID
		serviceID = build.ServiceID
		if err := s.lockServiceTx(ctx, tx, build.ServiceID); err != nil {
			return err
		}
		build, err = s.buildRunByIDQuerier(ctx, tx, buildID)
		if err != nil {
			return err
		}
		if build.BuilderID != builderID {
			return errBuildNotOwned
		}
		if buildStateTerminal(build.State) {
			return nil
		}
		if build.State != buildStateRunning {
			return errBuildNotOwned
		}
		if commitSHA != build.CommitSHA {
			return errBuildCommitMismatch
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
				buildStateCancelled, "cancelled; late builder completion ignored", now, buildID, buildStateRunning,
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
			completed.State = buildStateCancelled
			return nil
		}
		now := time.Now().UTC()
		stateValue := buildStateFailed
		switch state {
		case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
			stateValue = buildStateSucceeded
		case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
			stateValue = buildStateSuperseded
		case platformv1.BuildState_BUILD_STATE_FAILED:
			stateValue = buildStateFailed
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
			stateValue, commitSHA, imageDigest, failureReason, now, builderID, buildID, buildStateRunning,
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

		if stateValue != buildStateSucceeded {
			toState := deploymentStateFailed
			reasonCode := reasonBuildFailed
			detail := firstNonEmpty(failureReason, "Build failed")
			if stateValue == buildStateSuperseded {
				toState = deploymentStateSuperseded
				reasonCode = reasonBuildSuperseded
				detail = firstNonEmpty(failureReason, "Build superseded")
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:          toState,
				Actor:            deploymentActor{Kind: deploymentCauseBuilder, ID: builderID},
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
			build.ServiceID, build.QueuedAt, buildStateQueued, buildStateRunning, buildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		if newerCount > 0 {
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:          deploymentStateSuperseded,
				Actor:            deploymentActor{Kind: deploymentCauseBuilder, ID: builderID},
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
		if !usePendingRollout && serviceVolumeName(service.Spec) != "" {
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
				ToState:        deploymentStateFailed,
				Actor:          deploymentActor{Kind: deploymentCauseSystem},
				ReasonCode:     reasonDeploymentFailed,
				Detail:         errVolumeRollingUnsupported.Error(),
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
		if usePendingRollout {
			if _, err := tx.ExecContext(ctx,
				`UPDATE service_rollouts
				    SET state = $1, image_digest = $2, build_id = $3
				  WHERE service_id = $4 AND rollout_generation = $5`,
				rolloutStateInProgress, imageDigest, build.ID, build.ServiceID, nextRolloutGeneration,
			); err != nil {
				return err
			}
		} else if err := s.insertServiceRolloutTx(ctx, tx, build.ServiceID, nextRolloutGeneration, service.SpecRevision, "build-success", build.ID, "", now); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
			ToState:           deploymentStateScheduling,
			Actor:             deploymentActor{Kind: deploymentCauseBuilder, ID: builderID},
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
		return s.bumpAllDesiredRevisionsTx(ctx, tx)
	})
	if err != nil || !changed {
		return buildCompletion{}, err
	}
	if rolloutScheduled && d.notifier != nil {
		allocations, err := s.listAllocationsByServiceID(ctx, serviceID)
		if err != nil {
			return buildCompletion{}, err
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
	if d.events != nil {
		_, err = d.events.Publish(ctx, environmentID)
	}
	return buildCompletion{Build: completed, Changed: changed, RolloutScheduled: rolloutScheduled}, err
}

func scanBuildRunRow(scanner interface{ Scan(...any) error }) (buildRunRecord, error) {
	var (
		rec        buildRunRecord
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
		return buildRunRecord{}, err
	}
	var err error
	rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) enqueueBuildTx(ctx context.Context, tx *sql.Tx, service serviceRecord, commitSHA string, actor deploymentActor) (buildRunRecord, error) {
	s := d.store
	spec := desiredSourceSpec(service.Spec)
	if spec == nil {
		return buildRunRecord{}, errServiceNotBuildable
	}
	if commitSHA == "" {
		return buildRunRecord{}, errors.New("commit sha is required")
	}
	binding, err := s.sourceBindingByServiceIDQuerier(ctx, tx, service.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	if binding.AccessState != sourceAccessStateAvailable {
		return buildRunRecord{}, errSourceStateNotReady
	}
	if time.Now().UTC().After(binding.FreshUntil) {
		return buildRunRecord{}, errSourceStateNotReady
	}
	revision, err := s.sourceRevisionByBindingAndCommitTx(ctx, tx, binding.ID, commitSHA)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	snapshot, err := s.sourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	if actor.Kind == "" {
		actor.Kind = deploymentCauseUser
	}
	return d.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, actor)
}

func (d *Delivery) enqueueBuildFromSourceStateTx(ctx context.Context, tx *sql.Tx, service serviceRecord, revision sourceRevisionRecord, snapshot sourceSnapshotRecord, buildRecipe *platformv1.BuildRecipe, actor deploymentActor) (buildRunRecord, error) {
	s := d.store
	// Build enqueue, claim, and completion all mutate the service's current
	// deployment. Take the same lock before reading rollout state so concurrent
	// webhook and user-triggered builds have one durable order.
	if err := s.lockServiceTx(ctx, tx, service.ID); err != nil {
		return buildRunRecord{}, err
	}
	service, err := s.serviceByIDInternalQuerier(ctx, tx, service.ID)
	if err != nil {
		return buildRunRecord{}, err
	}
	if revision.ID == "" || snapshot.ID == "" || !sourceSnapshotMatchesRevision(snapshot, revision) {
		return buildRunRecord{}, errSourceStateNotReady
	}
	if err := ensureReadySnapshot(snapshot); err != nil {
		return buildRunRecord{}, err
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $2,
		        finished_at = $3,
		        failure_reason = $4
		  WHERE service_id = $1
		    AND state = $5`,
		service.ID, buildStateSuperseded, now, "superseded by newer queued build", buildStateQueued,
	); err != nil {
		return buildRunRecord{}, err
	}

	targetGeneration := service.RolloutGeneration + 1
	var currentRolloutState string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&currentRolloutState); err != nil && err != sql.ErrNoRows {
		return buildRunRecord{}, err
	}
	if currentRolloutState == rolloutStatePendingBuild {
		targetGeneration = service.RolloutGeneration
	}
	rec := buildRunRecord{
		ID:                      mustID(),
		ServiceID:               service.ID,
		ProjectID:               service.ProjectID,
		EnvironmentID:           service.EnvironmentID,
		CommitSHA:               revision.CommitSHA,
		CommitMessage:           revision.CommitMessage,
		CommitAuthor:            revision.CommitAuthor,
		State:                   buildStateQueued,
		SourceRevisionID:        revision.ID,
		SourceSnapshotID:        snapshot.ID,
		SourceSnapshotDigest:    snapshot.Digest,
		TargetRolloutGeneration: targetGeneration,
		BuildRecipe:             cloneBuildRecipe(buildRecipe),
		QueuedAt:                now,
	}
	recipeJSON, err := marshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return buildRunRecord{}, err
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
		return buildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE id = $3`,
		rec.ID, now, service.ID,
	); err != nil {
		return buildRunRecord{}, err
	}
	if actor.Kind == "" {
		actor.Kind = deploymentCauseSystem
	}
	reasonCode := reasonBuildQueued
	detail := "Build queued"
	if actor.Kind == deploymentCauseWebhook {
		reasonCode = reasonWebhookPush
		detail = "Build queued from webhook"
	}
	if _, err := s.insertDeploymentTx(ctx, tx, service.ID, deploymentStateQueuedBuild, actor, reasonCode, detail, service.SpecRevision, rec.TargetRolloutGeneration, rec.ID, "", "", now); err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) claimNextBuild(ctx context.Context, builderID, builderName string, staleAfter time.Duration) (buildRunRecord, error) {
	s := d.store
	var rec buildRunRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rec = buildRunRecord{}
		now, err := databaseTime(ctx, tx)
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
		// Retrying job preparation must return the same lease, not strand a running
		// build and claim another one for the same worker.
		existing, err := scanBuildRunRow(tx.QueryRowContext(ctx, `SELECT `+buildRunSelectColumns+` FROM build_runs WHERE state = $1 AND builder_id = $2 ORDER BY started_at, id LIMIT 1`, buildStateRunning, builderID))
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
			buildStateQueued,
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
		rec.State = buildStateRunning
		rec.BuilderID = builderID
		rec.StartedAt = sql.NullTime{Time: now, Valid: true}
		if err := s.lockServiceTx(ctx, tx, rec.ServiceID); err != nil {
			return err
		}
		build, err := s.buildRunByIDQuerier(ctx, tx, rec.ID)
		if err != nil {
			return err
		}
		if build.State != buildStateQueued {
			rec = buildRunRecord{}
			return nil
		}
		dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, rec.ServiceID, rec.ID)
		if err != nil {
			return err
		}
		if ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
			rec = buildRunRecord{}
			return nil
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = $2,
			        builder_id = $3
			  WHERE id = $4 AND state = $5`,
			buildStateRunning, now, builderID, rec.ID, buildStateQueued,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			rec = buildRunRecord{}
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
			ToState:          deploymentStateBuilding,
			Actor:            deploymentActor{Kind: deploymentCauseBuilder, ID: builderID},
			ReasonCode:       reasonBuildStarted,
			Detail:           "Builder claimed the build",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) RecoverExpiredBuilds(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return d.store.withTx(ctx, func(tx *sql.Tx) error {
		now, err := databaseTime(ctx, tx)
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
		buildStateRunning, cutoff,
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
	now, err := databaseTime(ctx, tx)
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
			rec.ServiceID, rec.QueuedAt, buildStateQueued, buildStateRunning, buildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		nextState := buildStateQueued
		failureReason := ""
		startedAt := any(nil)
		if newerCount > 0 {
			nextState = buildStateSuperseded
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
			nextState, startedAt, buildStateSuperseded, now, failureReason, rec.ID,
		); err != nil {
			return err
		}
		transition := deploymentTransitionInput{
			ToState:          deploymentStateQueuedBuild,
			Actor:            deploymentActor{Kind: deploymentCauseSystem},
			ReasonCode:       reasonBuildRequeued,
			Detail:           "Builder heartbeat expired; build requeued",
			IgnoreIfTerminal: true,
		}
		if nextState == buildStateSuperseded {
			transition.ToState = deploymentStateSuperseded
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
