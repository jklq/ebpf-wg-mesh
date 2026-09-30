package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/registry"
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
			// A superseded build is not a cancelled one: the work finished but a newer
			// build owns the rollout, so the late image drops here.
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
		var artifact BuildArtifactRecord
		if stateValue == BuildStateSucceeded {
			if build.SourceRevisionID == "" || build.SourceSnapshotID == "" || build.SourceSnapshotDigest == "" {
				return errSourceStateNotReady
			}
			repository, manifestDigest, err := registry.SplitPinnedReference(imageDigest)
			if err != nil {
				return fmt.Errorf("builder reported an invalid image reference: %w", err)
			}
			artifact, err = s.insertBuildArtifactTx(ctx, tx, insertArtifactParams{
				ServiceID:            build.ServiceID,
				BuildID:              build.ID,
				Kind:                 BuildArtifactBuild,
				SourceSnapshotDigest: build.SourceSnapshotDigest,
				CommitSHA:            commitSHA,
				BuildRecipe:          build.BuildRecipe,
				BuilderVersion:       BuilderToolchainVersion,
				ImageRepository:      repository,
				ImageManifestDigest:  manifestDigest,
				BuildActorKind:       build.BuildActorKind,
				BuildActorID:         build.BuildActorID,
				CreatedAt:            now,
			})
			if err != nil {
				return err
			}
			var newerCount int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM build_runs
				  WHERE service_id = $1 AND queued_at > $2 AND state IN ($3, $4, $5)`,
				build.ServiceID, build.QueuedAt, BuildStateQueued, BuildStateRunning, BuildStateSucceeded,
			).Scan(&newerCount); err != nil {
				return err
			}
			if newerCount > 0 {
				stateValue = BuildStateSuperseded
				failureReason = "superseded by newer build"
			}
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        commit_sha = $2,
			        artifact_id = NULLIF($3, ''),
			        failure_reason = $4,
			        finished_at = $5,
			        lease_expires_at = NULL
			  WHERE id = $6 AND state = $7 AND builder_id = $8 AND owner_epoch = $9`,
			stateValue, commitSHA, artifact.ID, failureReason, now, buildID, BuildStateRunning, builderID, epoch,
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
		completed.ArtifactID = artifact.ID
		if artifact.ID != "" {
			completed.ImageDigest = artifact.ImageRef
			artifactCopy := artifact
			completed.Artifact = &artifactCopy
		}
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
			detail := firstNonEmpty(failureReason, "Build failed")
			if stateValue == BuildStateSuperseded {
				toState = DeploymentStateSuperseded
				reasonCode = reasonBuildSuperseded
				detail = firstNonEmpty(failureReason, "Build superseded")
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
		depID := ""
		if dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, build.ServiceID, build.ID); err != nil {
			return err
		} else if ok {
			depID = dep.ID
		}
		_, scheduled, err := d.scheduleSucceededArtifactTx(ctx, tx, service, artifact, commitSHA, build.ID, depID,
			deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID}, reasonBuildSucceeded, "Image ready; scheduling rollout", now)
		if err != nil {
			return err
		}
		rolloutScheduled = scheduled
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

// scheduleSucceededArtifactTx adopts a build artifact as the resolved image and schedules
// its rollout. Completion and skip-rebuild share it so a reused image rolls out like a
// fresh build. Empty depID skips the deployment transition but still schedules.
func (d *Delivery) scheduleSucceededArtifactTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, artifact BuildArtifactRecord, commitSHA, buildID, depID string, actor deploymentActor, reasonCode, detail string, now time.Time) (DeploymentRecord, bool, error) {
	s := d.store
	transition := func(input deploymentTransitionInput) (DeploymentRecord, error) {
		if depID == "" {
			return DeploymentRecord{}, nil
		}
		updated, err := s.applyDeploymentTransitionTx(ctx, tx, depID, input)
		if err != nil && errors.Is(err, sql.ErrNoRows) {
			return DeploymentRecord{}, nil
		}
		return updated, err
	}
	nextRolloutGeneration := service.RolloutGeneration
	var currentRolloutState string
	var currentRolloutSpec int64
	err := tx.QueryRowContext(ctx,
		`SELECT state, spec_revision FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&currentRolloutState, &currentRolloutSpec)
	usePendingRollout := err == nil && currentRolloutState == rolloutStatePendingBuild && currentRolloutSpec == service.SpecRevision
	if err != nil && err != sql.ErrNoRows {
		return DeploymentRecord{}, false, err
	}
	if !usePendingRollout && ServiceVolumeName(service.Spec) != "" {
		updated, err := transition(deploymentTransitionInput{
			ToState:       DeploymentStateFailed,
			Actor:         deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode:    reasonDeploymentFailed,
			Detail:        ErrVolumeRollingUnsupported.Error(),
			ArtifactID:    artifact.ID,
			HasArtifactID: true,
		})
		return updated, false, err
	}
	if !usePendingRollout && (currentRolloutState == rolloutStateInProgress || currentRolloutState == rolloutStatePendingBuild) {
		existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
		if err != nil {
			return DeploymentRecord{}, false, err
		}
		if _, err := d.prepareReplacementRolloutTx(ctx, tx, service, existing, now); err != nil {
			return DeploymentRecord{}, false, err
		}
	}
	if !usePendingRollout {
		nextRolloutGeneration++
	}
	if _, err := journal.ServiceRow(service.ID).Exec(ctx, tx,
		`UPDATE service_delivery_status
		    SET current_artifact_id = NULLIF($1, ''),
		        last_successful_commit_sha = NULLIF($2, ''),
		        current_rollout_generation = $3,
		        latest_build_id = $4,
		        updated_at = $5
		  WHERE service_id = $6`,
		artifact.ID, commitSHA, nextRolloutGeneration, buildID, now, service.ID,
	); err != nil {
		return DeploymentRecord{}, false, err
	}
	if usePendingRollout {
		if _, err := journal.RolloutRow(service.ID, nextRolloutGeneration).Exec(ctx, tx,
			`UPDATE service_rollouts
			    SET state = $1, artifact_id = NULLIF($2, ''), build_id = $3
			  WHERE service_id = $4 AND rollout_generation = $5`,
			rolloutStateInProgress, artifact.ID, buildID, service.ID, nextRolloutGeneration,
		); err != nil {
			return DeploymentRecord{}, false, err
		}
	} else if err := s.insertServiceRolloutTx(ctx, tx, service.ID, nextRolloutGeneration, service.SpecRevision, "build-success", buildID, "", now); err != nil {
		return DeploymentRecord{}, false, err
	}
	updated, err := transition(deploymentTransitionInput{
		ToState:           DeploymentStateScheduling,
		Actor:             actor,
		ReasonCode:        reasonCode,
		Detail:            detail,
		ArtifactID:        artifact.ID,
		HasArtifactID:     true,
		RolloutGeneration: nextRolloutGeneration,
		HasRollout:        true,
		SpecRevision:      service.SpecRevision,
		HasSpecRevision:   true,
		IgnoreIfTerminal:  true,
	})
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	if _, err := d.advanceRolloutTx(ctx, tx, service.ID, now); err != nil {
		return DeploymentRecord{}, false, err
	}
	return updated, true, nil
}
