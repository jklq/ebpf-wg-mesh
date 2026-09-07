package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type queuedSourceBuild struct {
	Service ServiceRecord
	Build   BuildRunRecord
}

// queueSourceBuild atomically r	esolves the durable source snapshot and queues
// the deployment build. Publication happens only after the transaction commits.
func (d *Delivery) QueueSourceBuild(ctx context.Context, binding SourceBindingRecord, commitSHA string, pendingSnapshot SourceSnapshotRecord) (queuedSourceBuild, error) {
	var result queuedSourceBuild
	err := d.store.withTx(ctx, func(tx *sql.Tx) error {
		revision, err := d.store.sourceRevisionByBindingAndCommitTx(ctx, tx, binding.ID, commitSHA)
		if err != nil {
			return err
		}
		snapshot, err := d.store.sourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
		if errors.Is(err, sql.ErrNoRows) {
			if pendingSnapshot.ObjectKey == "" {
				return err
			}
			snapshot, err = d.store.upsertSourceSnapshotTx(ctx, tx, pendingSnapshot)
		}
		if err != nil {
			return err
		}
		service, err := d.store.serviceByIDInternalQuerier(ctx, tx, binding.ServiceID)
		if err != nil {
			return err
		}
		build, err := d.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, deploymentActor{Kind: DeploymentCauseWebhook})
		if err != nil {
			return err
		}
		result = queuedSourceBuild{Service: service, Build: build}
		return nil
	})
	if err != nil {
		return queuedSourceBuild{}, err
	}
	if d.events != nil {
		if _, err := d.events.Publish(ctx, result.Service.EnvironmentID); err != nil {
			return queuedSourceBuild{}, fmt.Errorf("publish source event: %w", err)
		}
	}
	return result, nil
}
