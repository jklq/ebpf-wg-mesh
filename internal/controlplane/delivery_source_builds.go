package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type queuedSourceBuild struct {
	Service serviceRecord
	Build   buildRunRecord
}

// queueSourceBuild atomically r	esolves the durable source snapshot and queues
// the deployment build. Publication happens only after the transaction commits.
func (d *Delivery) queueSourceBuild(ctx context.Context, binding sourceBindingRecord, commitSHA string, pendingSnapshot sourceSnapshotRecord) (queuedSourceBuild, error) {
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
		build, err := d.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, deploymentActor{Kind: deploymentCauseWebhook})
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
