package delivery

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/google/uuid"
)

// errSourceRevisionSuperseded: an unproven build request. No work is created.
var errSourceRevisionSuperseded = errors.New("source revision superseded by a newer observed revision")

// errSourceRevisionChainUnproven refuses a push chaining to neither the proven head nor a
// fetched one. Staleness is not provable from that, so the tracked head is reconciled
// instead of dropping the push.
var errSourceRevisionChainUnproven = errors.New("source revision push chain cannot prove currency")

// supersedeQueuedBuildsTx retires still-queued builds so only the newest request proceeds.
func supersedeQueuedBuildsTx(ctx context.Context, tx *sql.Tx, serviceID string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $2,
		        finished_at = $3,
		        failure_reason = $4
		  WHERE service_id = $1
		    AND state = $5`,
		serviceID, BuildStateSuperseded, now, "superseded by newer queued build", BuildStateQueued,
	)
	return err
}

// enqueueBuildFromSourceStateTx queues a build for verified source state, or skips the
// build when the same source already produced an image: the artifact rolls out with the
// current spec instead. Only current requests are served; older revisions are refused
// before they can supersede newer work or regress the rollout. Reuse returns reused=true.
func (d *Delivery) enqueueBuildFromSourceStateTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, revision source.SourceRevisionRecord, snapshot source.SourceSnapshotRecord, buildRecipe *platformv1.BuildRecipe, actor deploymentActor, transition source.BuildTransition) (BuildRunRecord, DeploymentRecord, bool, error) {
	return d.enqueueSourceBuildTx(ctx, tx, service, revision, snapshot, buildRecipe, actor, transition, false)
}

// Historical requests are authorized and fenced by QueueDeploymentRebuild. They
// deliberately build an old commit without changing the binding's proven head.
func (d *Delivery) enqueueSourceBuildTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, revision source.SourceRevisionRecord, snapshot source.SourceSnapshotRecord, buildRecipe *platformv1.BuildRecipe, actor deploymentActor, transition source.BuildTransition, historical bool) (BuildRunRecord, DeploymentRecord, bool, error) {
	s := d.store
	if err := s.lockServiceTx(ctx, tx, service.ID); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	service, err := s.serviceByIDInternalQuerier(ctx, tx, service.ID)
	if err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if err := requireLiveService(service.Deletion); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if revision.ID == "" || snapshot.ID == "" || !sourceSnapshotMatchesRevision(snapshot, revision) {
		return BuildRunRecord{}, DeploymentRecord{}, false, errSourceStateNotReady
	}
	if err := source.EnsureReadySnapshot(snapshot); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if actor.Kind == "" {
		actor.Kind = DeploymentCauseSystem
	}

	now := time.Now().UTC()
	// Freshness fence: the request must prove currency — its revision is the proven head,
	// advances from it, or was just fetched as the tracked head (see source.BuildTransition).
	// Older revisions carry no proof and are refused. Arrival order is not push order, so
	// currency is proven against the head, never against "latest observed".
	if !historical {
		head, err := s.sourceStore.SourceBindingHeadCommitTx(ctx, tx, revision.SourceBindingID)
		if err != nil {
			return BuildRunRecord{}, DeploymentRecord{}, false, err
		}
		if !transition.ProvesCurrent(revision.CommitSHA, head) {
			if transition.PreviousCommit != "" {
				// A push that cannot prove currency here at all (see errSourceRevisionChainUnproven):
				// the coordinator reconciles the head and queues the commit still current.
				return BuildRunRecord{}, DeploymentRecord{}, false, errSourceRevisionChainUnproven
			}
			return BuildRunRecord{}, DeploymentRecord{}, false, errSourceRevisionSuperseded
		}
		if head != revision.CommitSHA {
			if err := s.sourceStore.SetSourceBindingHeadCommitTx(ctx, tx, revision.SourceBindingID, revision.CommitSHA); err != nil {
				return BuildRunRecord{}, DeploymentRecord{}, false, err
			}
		}
	}

	artifact, ok, err := s.buildArtifactByReuseKeyTx(ctx, tx, service.ID, snapshot.Digest, buildRecipe)
	if err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if err := supersedeQueuedBuildsTx(ctx, tx, service.ID, now); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if ok {
		dep, err := d.reuseBuildArtifactTx(ctx, tx, service, revision, artifact, actor, now)
		if err != nil {
			return BuildRunRecord{}, DeploymentRecord{}, false, err
		}
		return BuildRunRecord{}, dep, true, nil
	}

	targetGeneration := service.RolloutGeneration + 1
	var currentRolloutState string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&currentRolloutState); err != nil && err != sql.ErrNoRows {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if currentRolloutState == rolloutStatePendingBuild {
		targetGeneration = service.RolloutGeneration
	}
	scheduler := d.buildScheduler
	rec := BuildRunRecord{
		ID:                      uuid.NewString(),
		ServiceID:               service.ID,
		ProjectID:               service.ProjectID,
		EnvironmentID:           service.EnvironmentID,
		CommitSHA:               revision.CommitSHA,
		CommitMessage:           revision.CommitMessage,
		CommitContributors:      revision.CommitContributors,
		State:                   BuildStateQueued,
		AttemptLimit:            scheduler.AttemptLimit,
		SourceRevisionID:        revision.ID,
		SourceSnapshotID:        snapshot.ID,
		SourceSnapshotDigest:    snapshot.Digest,
		BuildActorKind:          actor.Kind,
		BuildActorID:            actor.ID,
		TargetRolloutGeneration: targetGeneration,
		BuildRecipe:             source.CloneBuildRecipe(buildRecipe),
		QueuedAt:                now,
	}
	recipeJSON, err := source.MarshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_runs(
			id, service_id, commit_sha, commit_message, commit_contributors, state,
			source_revision_id, source_snapshot_id, source_snapshot_digest, target_rollout_generation, build_recipe_json,
			build_actor_kind, build_actor_id, builder_id, queued_at, attempt_limit
		) VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11, $12, $13, NULL, $14, $15)`,
		rec.ID, rec.ServiceID, rec.CommitSHA, rec.CommitMessage, rec.CommitContributors, rec.State,
		rec.SourceRevisionID, rec.SourceSnapshotID, rec.SourceSnapshotDigest, rec.TargetRolloutGeneration, recipeJSON,
		rec.BuildActorKind, rec.BuildActorID, rec.QueuedAt, rec.AttemptLimit,
	); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	if _, err := journal.ServiceRow(service.ID).Exec(ctx, tx,
		`UPDATE service_delivery_status
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE service_id = $3`,
		rec.ID, now, service.ID,
	); err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	reasonCode := reasonBuildQueued
	detail := "Build queued"
	if actor.Kind == DeploymentCauseWebhook {
		reasonCode = reasonWebhookPush
		detail = "Build queued from webhook"
	}
	dep, err := s.insertDeploymentTx(ctx, tx, service.ID, DeploymentStateQueuedBuild, actor, reasonCode, detail, service.SpecRevision, rec.TargetRolloutGeneration, rec.ID, "", "", now)
	if err != nil {
		return BuildRunRecord{}, DeploymentRecord{}, false, err
	}
	return rec, dep, false, nil
}

// reuseBuildArtifactTx deploys an already-built image without queueing builder work. The
// reuse is recorded as the revision's build so later releases don't re-sync forever; when
// the service already runs this artifact at the current spec, no deployment is created.
// The deployment captures the current spec, so reused images run with current variables.
func (d *Delivery) reuseBuildArtifactTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, revision source.SourceRevisionRecord, artifact BuildArtifactRecord, actor deploymentActor, now time.Time) (DeploymentRecord, error) {
	s := d.store
	recipeJSON, err := source.MarshalBuildRecipe(artifact.BuildRecipe)
	if err != nil {
		return DeploymentRecord{}, err
	}
	var reuseBuildID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO build_runs(
			id, service_id, commit_sha, commit_message, commit_contributors, state,
			artifact_id, source_revision_id, source_snapshot_digest, build_recipe_json,
			build_actor_kind, build_actor_id, queued_at, started_at, finished_at
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13, $13
		 WHERE NOT EXISTS (SELECT 1 FROM build_runs WHERE source_revision_id = $8)
		 RETURNING id`,
		uuid.NewString(), service.ID, revision.CommitSHA, revision.CommitMessage, revision.CommitContributors, BuildStateSucceeded,
		artifact.ID, revision.ID, artifact.SourceSnapshotDigest, recipeJSON,
		actor.Kind, actor.ID, now,
	).Scan(&reuseBuildID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM build_runs WHERE source_revision_id = $1`,
			revision.ID,
		).Scan(&reuseBuildID); err != nil {
			return DeploymentRecord{}, err
		}
	case err != nil:
		return DeploymentRecord{}, err
	}
	if _, err := journal.ServiceRow(service.ID).Exec(ctx, tx,
		`UPDATE service_delivery_status
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE service_id = $3`,
		reuseBuildID, now, service.ID,
	); err != nil {
		return DeploymentRecord{}, err
	}
	current, ok, err := s.currentDeploymentTx(ctx, tx, service.ID)
	if err != nil {
		return DeploymentRecord{}, err
	}
	if ok && current.ArtifactID == artifact.ID && current.SpecRevision == service.SpecRevision &&
		current.State != DeploymentStateFailed && current.State != DeploymentStateCancelled &&
		current.State != DeploymentStateCrashed && current.State != DeploymentStateRemoved {

		return current, nil
	}
	dep, err := s.insertDeploymentTx(ctx, tx, service.ID, DeploymentStateStaged, actor, reasonBuildReused,
		"Reusing previously built image for commit "+shortSHA(revision.CommitSHA),
		service.SpecRevision, 0, reuseBuildID, artifact.ID, "", now)
	if err != nil {
		return DeploymentRecord{}, err
	}
	updated, _, err := d.scheduleSucceededArtifactTx(ctx, tx, service, artifact, revision.CommitSHA, reuseBuildID, dep.ID,
		actor, reasonBuildReused, "Reusing previously built image for commit "+shortSHA(revision.CommitSHA), now)
	if err != nil {
		return DeploymentRecord{}, err
	}
	return updated, nil
}

func (d *Delivery) serviceNeedsSourceBuildTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, autoDeploy bool) (bool, error) {
	if source.DesiredSourceSpec(service.Spec) == nil {
		return false, nil
	}
	if service.RolloutGeneration == 0 || strings.TrimSpace(service.ResolvedArtifactID) == "" {
		return true, nil
	}
	if !autoDeploy {
		pending, err := d.store.sourceStore.ServiceHasUnbuiltSourceRevisionTx(ctx, tx, service.ID)
		if err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
	}

	var deployedSpecRevision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spec_revision FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&deployedSpecRevision); err != nil {
		return false, err
	}
	deployedSpec, err := d.store.loadServiceDetailsQuerier(ctx, tx, service.ID, deployedSpecRevision)
	if err != nil {
		return false, err
	}
	return !sameDesiredSourceSpec(deployedSpec, service.Spec), nil
}

func (d *Delivery) QueueSourceBuild(ctx context.Context, binding source.SourceBindingRecord, commitSHA string, pendingSnapshot source.SourceSnapshotRecord, transition source.BuildTransition) (source.QueuedBuild, error) {
	var result source.QueuedBuild
	var service ServiceRecord
	var build BuildRunRecord
	var reused bool
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		revision, err := d.store.sourceStore.SourceRevisionByBindingAndCommitTx(ctx, tx, binding.ID, commitSHA)
		if err != nil {
			return err
		}
		snapshot, err := d.store.sourceStore.SourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
		if errors.Is(err, sql.ErrNoRows) {
			if pendingSnapshot.ObjectKey == "" {
				return err
			}
			snapshot, err = d.store.sourceStore.UpsertSourceSnapshotTx(ctx, tx, pendingSnapshot)
		}
		if err != nil {
			return err
		}
		service, err = d.store.serviceByIDInternalQuerier(ctx, tx, binding.ServiceID)
		if err != nil {
			return err
		}
		var dep DeploymentRecord
		build, dep, reused, err = d.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, deploymentActor{Kind: DeploymentCauseWebhook}, transition)
		if errors.Is(err, errSourceRevisionSuperseded) || errors.Is(err, errSourceRevisionChainUnproven) {
			// A late or retried older revision creates no work: the binding moved on. Deliberate
			// redeploys go through deployment actions; an unprovable chain is reported so the
			// coordinator reconciles the head and only the commit still current builds.
			result = source.QueuedBuild{Superseded: true, ChainUnproven: errors.Is(err, errSourceRevisionChainUnproven)}
			return nil
		}
		if err != nil {
			return err
		}
		result = source.QueuedBuild{BuildID: build.ID, DeploymentID: dep.ID, Reused: reused}
		if reused {
			result.BuildID = dep.BuildID
		}
		return nil
	})
	if err != nil {
		return source.QueuedBuild{}, err
	}
	if result.Superseded {
		return result, nil
	}
	scope := logs.ServiceScope{
		EnvironmentID:     service.EnvironmentID,
		ServiceID:         service.ID,
		RolloutGeneration: service.RolloutGeneration,
	}
	if reused {
		d.logEmitter.EmitBuildf(ctx, scope, result.BuildID, logs.StageBuild,
			"Reusing previously built image for commit %s on ref %s", shortSHA(commitSHA), binding.TrackedRef,
		)
		return result, nil
	}
	d.logEmitter.EmitBuildf(ctx, scope, build.ID, logs.StageBuild,
		"Queued build for commit %s on ref %s", shortSHA(build.CommitSHA), binding.TrackedRef,
	)
	return result, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
