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

func (d *Delivery) CompleteBuild(ctx context.Context, builderID, buildID string, epoch int64, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) (BuildCompletion, error) {
	s := d.store
	var serviceID string
	var changed, rolloutScheduled bool
	var completed BuildRunRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		if BuildStateTerminal(build.State) {
			return nil
		}
		if build.State != BuildStateRunning {
			return ErrBuildNotOwned
		}
		if build.BuilderID != builderID {
			return ErrBuildNotOwned
		}
		if build.OwnerEpoch != epoch {
			return ErrBuildLeaseLost
		}
		if commitSHA != build.CommitSHA {
			return ErrBuildCommitMismatch
		}
		completed = build
		deleted, err := s.serviceDeletionQuerier(ctx, tx, build.ServiceID)
		if err != nil {
			return err
		}
		dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, build.ServiceID, build.ID)
		if err != nil {
			return err
		}
		cancelRequested := build.CancelRequestedAt.Valid
		if deleted != nil || cancelRequested || ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
			// A superseded build is not a cancelled one: the work finished
			// but a newer build owns the rollout. Either way the late image
			// is dropped here — no runtime identity update, no rollout.
			targetState := BuildStateCancelled
			attemptOutcome := BuildAttemptCancelled
			reason := "cancelled; late builder completion ignored"
			if deleted != nil {
				reason = "cancelled; service deleted"
			} else if cancelRequested {
				reason = "cancelled by user"
			} else if ok && !dep.IsCurrent {
				targetState = BuildStateSuperseded
				attemptOutcome = BuildAttemptSuperseded
				reason = "superseded by newer build"
			}
			now := time.Now().UTC()
			result, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3, lease_expires_at = NULL
				  WHERE id = $4 AND state = $5 AND builder_id = $6 AND owner_epoch = $7`,
				targetState, reason, now, buildID, BuildStateRunning, builderID, epoch,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return ErrBuildLeaseLost
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE builder_workers SET current_build_id = '', last_heartbeat_at = $1, updated_at = $1 WHERE id = $2`,
				now, builderID,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, attemptOutcome, reason, buildID, build.AttemptCount,
			); err != nil {
				return err
			}
			changed = true
			completed.State = targetState
			completed.FailureReason = reason
			completed.FinishedAt = sql.NullTime{Time: now, Valid: true}
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
			        lease_expires_at = NULL
			  WHERE id = $6 AND state = $7 AND builder_id = $8 AND owner_epoch = $9`,
			stateValue, commitSHA, imageDigest, failureReason, now, buildID, BuildStateRunning, builderID, epoch,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrBuildLeaseLost
		}
		changed = true
		completed.State = stateValue
		completed.ImageDigest = imageDigest
		completed.FailureReason = failureReason
		completed.FinishedAt = sql.NullTime{Time: now, Valid: true}
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
		attemptOutcome := BuildAttemptSucceeded
		if stateValue == BuildStateFailed {
			attemptOutcome = BuildAttemptFailedTerminal
		} else if stateValue == BuildStateSuperseded {
			attemptOutcome = BuildAttemptSuperseded
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
			  WHERE build_id = $4 AND attempt_number = $5`,
			now, attemptOutcome, failureReason, buildID, build.AttemptCount,
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
			`UPDATE service_delivery_status
			    SET current_resolved_image = NULLIF($1, ''),
			        last_successful_commit_sha = NULLIF($2, ''),
			        current_rollout_generation = $3,
			        latest_build_id = $4,
			        updated_at = $5
			  WHERE service_id = $6`,
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
		&rec.OwnerEpoch,
		&rec.LeaseExpiresAt,
		&rec.AttemptCount,
		&rec.AttemptLimit,
		&rec.CancelRequestedAt,
		&rec.CancelRequestedBy,
		&rec.DeadlineAt,
		&rec.LastHeartbeatAt,
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

func scanBuildAttemptRow(scanner interface{ Scan(...any) error }) (BuildAttemptRecord, error) {
	var rec BuildAttemptRecord
	err := scanner.Scan(
		&rec.ID,
		&rec.BuildID,
		&rec.AttemptNumber,
		&rec.BuilderID,
		&rec.OwnerEpoch,
		&rec.StartedAt,
		&rec.FinishedAt,
		&rec.Outcome,
		&rec.Detail,
	)
	return rec, err
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
	if err := requireLiveService(service.Deletion); err != nil {
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
	scheduler := d.BuildSchedulerConfig()
	rec := BuildRunRecord{
		ID:                      uuid.NewString(),
		ServiceID:               service.ID,
		ProjectID:               service.ProjectID,
		EnvironmentID:           service.EnvironmentID,
		CommitSHA:               revision.CommitSHA,
		CommitMessage:           revision.CommitMessage,
		CommitAuthor:            revision.CommitAuthor,
		State:                   BuildStateQueued,
		AttemptLimit:            scheduler.AttemptLimit,
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
			builder_id, queued_at, attempt_limit
		) VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11, NULL, $12, $13)`,
		rec.ID, rec.ServiceID, rec.CommitSHA, rec.CommitMessage, rec.CommitAuthor, rec.State,
		rec.SourceRevisionID, rec.SourceSnapshotID, rec.SourceSnapshotDigest, rec.TargetRolloutGeneration, recipeJSON, rec.QueuedAt, rec.AttemptLimit,
	); err != nil {
		return BuildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_delivery_status
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE service_id = $3`,
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

func (d *Delivery) ClaimNextBuild(ctx context.Context, builderID, builderName string) (BuildRunRecord, error) {
	s := d.store
	scheduler := d.BuildSchedulerConfig()
	var rec BuildRunRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec = BuildRunRecord{}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO builder_workers(id, name, current_build_id, last_heartbeat_at, created_at, updated_at, drained)
			 VALUES ($1, $2, '', $3, $3, $3, FALSE)
			 ON CONFLICT(id) DO UPDATE
			    SET name = excluded.name,
			        last_heartbeat_at = excluded.last_heartbeat_at,
			        updated_at = excluded.updated_at`,
			builderID, builderName, now,
		); err != nil {
			return err
		}
		if err := d.recoverExpiredBuildsTx(ctx, tx, now, scheduler); err != nil {
			return err
		}
		// Resuming an owned running build is not a new claim: a drained
		// builder or a paused scheduler still hands back the builder's own
		// work so running builds finish instead of stalling until the lease
		// expires.
		existing, err := scanBuildRunRow(tx.QueryRowContext(ctx, `SELECT `+buildRunSelectColumns+` FROM build_runs WHERE state = $1 AND builder_id = $2 ORDER BY started_at, id LIMIT 1`, BuildStateRunning, builderID))
		if err == nil {
			rec, err = s.buildRunByIDQuerier(ctx, tx, existing.ID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var drained bool
		if err := tx.QueryRowContext(ctx, `SELECT drained FROM builder_workers WHERE id = $1`, builderID).Scan(&drained); err != nil {
			return err
		}
		if drained {
			return nil
		}
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM build_scheduler_control WHERE id = TRUE`).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return nil
		}
		var globalRunning int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state = $1`, BuildStateRunning).Scan(&globalRunning); err != nil {
			return err
		}
		if globalRunning >= scheduler.MaxConcurrentGlobal {
			return nil
		}
		candidates, err := listClaimCandidatesTx(ctx, tx, 20)
		if err != nil {
			return err
		}
		for _, candidateID := range candidates {
			claimed, ok, err := d.tryClaimBuildTx(ctx, tx, candidateID, builderID, now, scheduler)
			if err != nil {
				return err
			}
			if ok {
				rec = claimed
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func listClaimCandidatesTx(ctx context.Context, tx *sql.Tx, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT b.id
		   FROM build_runs b
		  WHERE b.state = $1
		    AND NOT EXISTS (
		        SELECT 1 FROM services s
		         JOIN environments e ON e.id = s.environment_id
		         JOIN projects p ON p.id = e.project_id
		        WHERE s.id = b.service_id
		          AND (s.deleted_at IS NOT NULL OR e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL)
		    )
		  ORDER BY b.queued_at ASC, b.id ASC
		  LIMIT $2`,
		BuildStateQueued, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *Delivery) tryClaimBuildTx(ctx context.Context, tx *sql.Tx, buildID, builderID string, now time.Time, scheduler BuildSchedulerConfig) (BuildRunRecord, bool, error) {
	s := d.store
	serviceID, err := serviceIDForBuildTx(ctx, tx, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BuildRunRecord{}, false, nil
		}
		return BuildRunRecord{}, false, err
	}
	if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
		return BuildRunRecord{}, false, err
	}
	build, err := s.buildRunByIDQuerier(ctx, tx, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BuildRunRecord{}, false, nil
		}
		return BuildRunRecord{}, false, err
	}
	if build.State != BuildStateQueued {
		return BuildRunRecord{}, false, nil
	}
	if build.AttemptCount >= build.AttemptLimit {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3 WHERE id = $4 AND state = $5`,
			BuildStateFailed, "attempt limit exhausted before claim", now, buildID, BuildStateQueued,
		); err != nil {
			return BuildRunRecord{}, false, err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, build.ServiceID, build.ID, deploymentTransitionInput{
			ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonBuildFailed, Detail: "Build retry budget exhausted",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return BuildRunRecord{}, false, err
		}
		return BuildRunRecord{}, false, nil
	}
	if deletion, err := s.serviceDeletionQuerier(ctx, tx, build.ServiceID); err != nil {
		return BuildRunRecord{}, false, err
	} else if deletion != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3 WHERE id = $4 AND state = $5`,
			BuildStateCancelled, "service deleted", now, buildID, BuildStateQueued,
		); err != nil {
			return BuildRunRecord{}, false, err
		}
		return BuildRunRecord{}, false, nil
	}
	dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, build.ServiceID, build.ID)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
		return BuildRunRecord{}, false, nil
	}
	var projectRunning int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM build_runs b
		   JOIN services s ON s.id = b.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE b.state = $1 AND e.project_id = $2`,
		BuildStateRunning, build.ProjectID,
	).Scan(&projectRunning); err != nil {
		return BuildRunRecord{}, false, err
	}
	if projectRunning >= scheduler.MaxConcurrentPerProject {
		return BuildRunRecord{}, false, nil
	}
	leaseExpires := now.Add(scheduler.LeaseTTL)
	deadline := now.Add(scheduler.BuildTimeout)
	result, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $1,
		        started_at = $2,
		        builder_id = $3,
		        owner_epoch = owner_epoch + 1,
		        attempt_count = attempt_count + 1,
		        lease_expires_at = $4,
		        deadline_at = $5,
		        last_heartbeat_at = $2
		  WHERE id = $6 AND state = $7`,
		BuildStateRunning, now, builderID, leaseExpires, deadline, buildID, BuildStateQueued,
	)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if affected != 1 {
		return BuildRunRecord{}, false, nil
	}
	claimed, err := s.buildRunByIDQuerier(ctx, tx, buildID)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_attempts(id, build_id, attempt_number, builder_id, owner_epoch, started_at, outcome)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), buildID, claimed.AttemptCount, builderID, claimed.OwnerEpoch, now, BuildAttemptLeased,
	); err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = $1,
		        last_heartbeat_at = $2,
		        updated_at = $2
		  WHERE id = $3`,
		buildID, now, builderID,
	); err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, claimed.ServiceID, claimed.ID, deploymentTransitionInput{
		ToState:          DeploymentStateBuilding,
		Actor:            deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
		ReasonCode:       reasonBuildStarted,
		Detail:           "Builder claimed the build",
		IgnoreIfTerminal: true,
	}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BuildRunRecord{}, false, err
	}
	return claimed, true, nil
}

func serviceIDForBuildTx(ctx context.Context, tx *sql.Tx, buildID string) (string, error) {
	var serviceID string
	if err := tx.QueryRowContext(ctx, `SELECT service_id FROM build_runs WHERE id = $1`, buildID).Scan(&serviceID); err != nil {
		return "", err
	}
	return serviceID, nil
}

func (d *Delivery) RecoverExpiredBuilds(ctx context.Context) error {
	scheduler := d.BuildSchedulerConfig()
	return d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return d.recoverExpiredBuildsTx(ctx, tx, now, scheduler)
	})
}

func (d *Delivery) recoverExpiredBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time, scheduler BuildSchedulerConfig) error {
	if err := d.expireQueuedBuildsTx(ctx, tx, now, scheduler); err != nil {
		return err
	}
	if err := d.timeoutRunningBuildsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := d.requeueLeaseExpiredBuildsTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers w
		    SET current_build_id = '',
		        updated_at = $1
		  WHERE w.current_build_id <> ''
		    AND NOT EXISTS (
		        SELECT 1 FROM build_runs b
		         WHERE b.id = w.current_build_id
		           AND b.state = $2
		           AND b.builder_id = w.id
		    )`,
		now, BuildStateRunning,
	); err != nil {
		return err
	}
	return nil
}

func (d *Delivery) expireQueuedBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time, scheduler BuildSchedulerConfig) error {
	s := d.store
	cutoff := now.Add(-scheduler.MaxQueueAge)
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id FROM build_runs WHERE state = $1 AND queued_at < $2 ORDER BY queued_at ASC, id ASC LIMIT 100`,
		BuildStateQueued, cutoff,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type queued struct{ ID, ServiceID string }
	var expired []queued
	for rows.Next() {
		var rec queued
		if err := rows.Scan(&rec.ID, &rec.ServiceID); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3
			  WHERE id = $4 AND state = $5`,
			BuildStateFailed, "build waited longer than the maximum queue age", now, rec.ID, BuildStateQueued,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonBuildFailed, Detail: "Build expired in queue",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}

// timeoutRunningBuildsTx fails builds past their claim deadline. A timeout is
// terminal even with retry budget left: a build that overruns once will
// almost surely overrun again, so retry is an operator decision, not a loop.
func (d *Delivery) timeoutRunningBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id, attempt_count FROM build_runs
		  WHERE state = $1 AND deadline_at IS NOT NULL AND deadline_at <= $2
		  ORDER BY deadline_at ASC, id ASC LIMIT 100`,
		BuildStateRunning, now,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type timedOut struct {
		ID           string
		ServiceID    string
		AttemptCount int64
	}
	var expired []timedOut
	for rows.Next() {
		var rec timedOut
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.AttemptCount); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3, lease_expires_at = NULL
			  WHERE id = $4 AND state = $5`,
			BuildStateFailed, "build timeout exceeded", now, rec.ID, BuildStateRunning,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
			  WHERE build_id = $4 AND attempt_number = $5`,
			now, BuildAttemptTimedOut, "build timeout exceeded", rec.ID, rec.AttemptCount,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonBuildFailed, Detail: "Build timeout exceeded",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}

func (d *Delivery) requeueLeaseExpiredBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id, queued_at, attempt_count, attempt_limit, cancel_requested_at IS NOT NULL
		   FROM build_runs
		  WHERE state = $1 AND lease_expires_at IS NOT NULL AND lease_expires_at <= $2
		  ORDER BY queued_at ASC, id ASC LIMIT 100`,
		BuildStateRunning, now,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	type expiredBuild struct {
		ID              string
		ServiceID       string
		QueuedAt        time.Time
		AttemptCount    int64
		AttemptLimit    int64
		CancelRequested bool
	}
	var expired []expiredBuild
	for rows.Next() {
		var rec expiredBuild
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.QueuedAt, &rec.AttemptCount, &rec.AttemptLimit, &rec.CancelRequested); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		if rec.CancelRequested {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3,
				        builder_id = NULL, lease_expires_at = NULL
				  WHERE id = $4 AND state = $5`,
				BuildStateCancelled, "cancelled by user", now, rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptCancelled, "cancelled while worker was lost", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			continue
		}
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
		if newerCount > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, finished_at = $2, builder_id = NULL,
				        lease_expires_at = NULL, failure_reason = $3
				  WHERE id = $4 AND state = $5`,
				BuildStateSuperseded, now, "superseded after worker loss", rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptSuperseded, "superseded after worker loss", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
				ToState: DeploymentStateSuperseded, Actor: deploymentActor{Kind: DeploymentCauseSystem},
				ReasonCode: reasonBuildSuperseded, Detail: "Superseded after worker loss",
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
				return err
			}
			continue
		}
		if rec.AttemptCount >= rec.AttemptLimit {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, finished_at = $2, builder_id = NULL,
				        lease_expires_at = NULL, failure_reason = $3
				  WHERE id = $4 AND state = $5`,
				BuildStateFailed, now, "retry budget exhausted after worker loss", rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptWorkerLost, "retry budget exhausted", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
				ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
				ReasonCode: reasonBuildFailed, Detail: "Build retry budget exhausted after worker loss",
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = NULL,
			        builder_id = NULL,
			        lease_expires_at = NULL,
			        deadline_at = NULL,
			        last_heartbeat_at = NULL,
			        failure_reason = ''
			  WHERE id = $2 AND state = $3`,
			BuildStateQueued, rec.ID, BuildStateRunning,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
			  WHERE build_id = $4 AND attempt_number = $5`,
			now, BuildAttemptWorkerLost, "worker lease expired; build requeued", rec.ID, rec.AttemptCount,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState:          DeploymentStateQueuedBuild,
			Actor:            deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode:       reasonBuildRequeued,
			Detail:           "Builder lease expired; build requeued",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}
