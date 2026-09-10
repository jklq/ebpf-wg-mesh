package delivery

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"
)

func (d *Delivery) QueueSourceBuild(ctx context.Context, binding source.SourceBindingRecord, commitSHA string, pendingSnapshot source.SourceSnapshotRecord) (source.QueuedBuild, error) {
	var result source.QueuedBuild
	var service ServiceRecord
	var build BuildRunRecord
	err := d.store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		build, err = d.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, deploymentActor{Kind: DeploymentCauseWebhook})
		if err != nil {
			return err
		}
		result = source.QueuedBuild{BuildID: build.ID}
		return nil
	})
	if err != nil {
		return source.QueuedBuild{}, err
	}
	d.logEmitter.EmitBuildf(ctx, logs.ServiceScope{
		EnvironmentID:     service.EnvironmentID,
		ServiceID:         service.ID,
		RolloutGeneration: service.RolloutGeneration,
	}, build.ID, logs.StageBuild,
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
