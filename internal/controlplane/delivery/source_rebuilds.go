package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/source"
)

func (d *Delivery) retainedDeploymentImageTx(ctx context.Context, tx *sql.Tx, target DeploymentRecord) (bool, error) {
	if target.ArtifactID == "" {
		return false, nil
	}
	artifact, err := d.store.buildArtifactByIDQuerier(ctx, tx, target.ArtifactID)
	return artifact.ImageRetained, err
}

func (d *Delivery) rebuildHistoricalDeploymentTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, target DeploymentRecord, userID, reason string) (string, error) {
	if source.DesiredSourceSpec(target.ResolvedSpec) == nil {
		return "", fmt.Errorf("%w: this image is no longer retained and the deployment has no GitHub source to rebuild; deploy an available image instead", ErrDeploymentActionInvalid)
	}
	revisionID := target.SourceRevisionID
	if revisionID == "" && target.BuildID != "" {
		build, err := d.store.buildRunByIDQuerier(ctx, tx, target.BuildID)
		if err != nil {
			return "", err
		}
		revisionID = build.SourceRevisionID
	}
	if revisionID == "" {
		return "", fmt.Errorf("%w: historical source metadata is unavailable; deploy an available image instead", ErrDeploymentActionInvalid)
	}
	revision, err := d.store.sourceStore.SourceRevisionByIDTx(ctx, tx, revisionID)
	if err != nil {
		return "", err
	}
	return d.stageSourceDeploymentTx(ctx, tx, service, target, userID, reason,
		"Fetching GitHub source at commit "+revision.CommitSHA+" for a best effort rebuild", revision.ID)
}

func (d *Delivery) DeploymentRebuildRequest(ctx context.Context, deploymentID, revisionID string) (source.RebuildRequest, bool, error) {
	s := d.store
	dep, err := s.queryDeploymentRow(ctx, s.db, `SELECT `+deploymentSelectColumns+` FROM deployments WHERE id = $1`, deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return source.RebuildRequest{}, false, nil
	}
	if err != nil || !dep.IsCurrent || dep.State != DeploymentStateStaged {
		return source.RebuildRequest{}, false, err
	}
	if dep.SourceRevisionID != revisionID {
		return source.RebuildRequest{}, false, ErrDeploymentActionInvalid
	}
	service, err := s.serviceByIDInternalQuerier(ctx, s.db, dep.ServiceID)
	if err != nil || service.Deletion != nil {
		return source.RebuildRequest{}, false, err
	}
	revision, err := s.sourceStore.SourceRevisionByIDTx(ctx, s.db, revisionID)
	if err != nil {
		return source.RebuildRequest{}, false, err
	}
	if revision.ServiceID != dep.ServiceID {
		return source.RebuildRequest{}, false, ErrDeploymentActionInvalid
	}
	spec := source.DesiredSourceSpec(dep.ResolvedSpec)
	return source.RebuildRequest{DeploymentID: dep.ID, ServiceID: dep.ServiceID,
		ProjectID: service.ProjectID, RepositorySelector: spec.GetRepositorySelector(),
		Revision: revision, BuildRecipe: source.CloneBuildRecipe(spec.GetBuildRecipe())}, true, nil
}

func (d *Delivery) QueueDeploymentRebuild(ctx context.Context, req source.RebuildRequest, pending source.SourceSnapshotRecord) error {
	s := d.store
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := s.lockServiceTx(ctx, tx, req.ServiceID); err != nil {
			return err
		}
		dep, err := s.deploymentByIDTx(ctx, tx, req.DeploymentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || !dep.IsCurrent || dep.State != DeploymentStateStaged {
			return err
		}
		if dep.ServiceID != req.ServiceID || dep.SourceRevisionID != req.Revision.ID {
			return ErrDeploymentActionInvalid
		}
		service, err := s.serviceByIDInternalQuerier(ctx, tx, req.ServiceID)
		if err != nil || service.Deletion != nil {
			return err
		}
		snapshot, err := s.sourceStore.SourceSnapshotByRevisionIDTx(ctx, tx, req.Revision.ID)
		if errors.Is(err, sql.ErrNoRows) && pending.ObjectKey != "" {
			snapshot, err = s.sourceStore.UpsertSourceSnapshotTx(ctx, tx, pending)
		}
		if err != nil {
			return err
		}
		_, next, reused, err := d.enqueueSourceBuildTx(ctx, tx, service, req.Revision, snapshot, req.BuildRecipe,
			deploymentActor{Kind: DeploymentCauseUser, ID: dep.CauseID}, source.BuildTransition{}, true)
		if err != nil {
			return err
		}
		// The ordinary enqueue path creates a new deployment; reconnect the
		// action to its queued result so idempotent requests find that result.
		if _, err := tx.ExecContext(ctx, `UPDATE deployment_actions SET result_deployment_id = $1
			WHERE result_deployment_id = $2`, next.ID, dep.ID); err != nil {
			return err
		}
		detail := "Historical source rebuild queued for commit " + req.Revision.CommitSHA
		if reused {
			detail = "Historical source matches a retained image"
		}
		if _, err := journal.DeploymentRow(next.ID).Exec(ctx, tx,
			`UPDATE deployments SET reason_code = $1, detail = $2 WHERE id = $3`, dep.ReasonCode, detail, next.ID); err != nil {
			return err
		}
		return nil
	})
}

func (d *Delivery) FailDeploymentRebuild(ctx context.Context, deploymentID, reason string) error {
	s := d.store
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		dep, err := s.deploymentByIDTx(ctx, tx, deploymentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || !dep.IsCurrent || dep.State != DeploymentStateStaged {
			return err
		}
		if _, err := s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
			ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonBuildFailed, Detail: reason,
		}); err != nil {
			return err
		}
		_, err = journal.RolloutRow(dep.ServiceID, dep.RolloutGeneration).Exec(ctx, tx,
			`UPDATE service_rollouts SET state = $1, failure_reason = $2, completed_at = $3
			 WHERE service_id = $4 AND rollout_generation = $5`, rolloutStateFailed, reason, time.Now().UTC(), dep.ServiceID, dep.RolloutGeneration)
		return err
	})
}
