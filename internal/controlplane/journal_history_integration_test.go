//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestJournalKeepsOperationalClosureWithoutResidentHistory(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	store.journal.SetVerifyRecordings(true)
	initial, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	allocation := allocationForGeneration(t, store, service.ID, 1)[0]
	deploymentID := initial.Assignments[allocation.ID].DeploymentID
	const latest = 129
	historyID := func(n int) string { return fmt.Sprintf("%s/history/%d", service.ID, n) }
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for n := 2; n <= latest; n++ {
			if err := journal.Rows(journal.RevisionRow(service.ID, int64(n)), journal.RolloutRow(service.ID, int64(n)), journal.DeploymentRow(historyID(n))).Capture(ctx); err != nil {
				return err
			}
		}
		for _, query := range []string{
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, env_dek_id, env_ciphertext, created_at)
			 SELECT service_id, n, spec_json, env_dek_id, env_ciphertext, created_at
			 FROM service_revisions CROSS JOIN generate_series(2, 129) n WHERE service_id = $1 AND spec_revision = 1`,
			`INSERT INTO service_rollouts(service_id, rollout_generation, spec_revision, reason, state, strategy_json, desired_replica_count, artifact_id, completed_at, progress_at, created_at)
			 SELECT service_id, n, n, reason, 'succeeded', strategy_json, desired_replica_count, artifact_id, completed_at, progress_at, created_at
			 FROM service_rollouts CROSS JOIN generate_series(2, 129) n WHERE service_id = $1 AND rollout_generation = 1`,
			`INSERT INTO deployments(id, service_id, spec_revision, rollout_generation, artifact_id, state, cause_kind, reason_code, created_at, updated_at)
			 SELECT service_id || '/history/' || n, service_id, n, n, artifact_id, 'superseded', cause_kind, reason_code, created_at, updated_at
			 FROM deployments CROSS JOIN generate_series(2, 129) n WHERE service_id = $1 AND is_current`,
		} {
			if _, err := tx.ExecContext(ctx, query, service.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertClosure := func(state journal.DurableState, count int) {
		t.Helper()
		if len(state.Revisions) != count || len(state.Rollouts) != count || len(state.Deployments) != count {
			t.Fatalf("resident rows: revisions=%d rollouts=%d deployments=%d, want %d each", len(state.Revisions), len(state.Rollouts), len(state.Deployments), count)
		}
	}
	resident, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertClosure(resident, 1)
	if resident.LogIndex != initial.LogIndex {
		t.Fatal("inserting nonoperational history advanced the journal")
	}
	replica := journal.New(store.db, "default", nil)
	rebuilt, err := replica.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(resident, rebuilt) {
		t.Fatalf("filtered SQL rebuild differs: %v", err)
	}
	// Activate an unchanged historical revision; the existing allocation still
	// references the old generation, whose full closure must stay resident.
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		updates := []struct {
			mutation journal.Mutation
			query    string
			args     []any
		}{
			{journal.DeploymentRow(deploymentID), `UPDATE deployments SET is_current = FALSE, state = 'superseded' WHERE id = $1`, []any{deploymentID}},
			{journal.DeploymentRow(historyID(latest)), `UPDATE deployments SET is_current = TRUE, state = 'active' WHERE id = $1`, []any{historyID(latest)}},
			{journal.ServiceRow(service.ID), `UPDATE services SET current_spec_revision = 129 WHERE id = $1`, []any{service.ID}},
			{journal.ServiceRow(service.ID), `UPDATE service_delivery_status SET current_rollout_generation = 129 WHERE service_id = $1`, []any{service.ID}},
		}
		for _, update := range updates {
			if _, err := update.mutation.Exec(ctx, tx, update.query, update.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resident, err = store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertClosure(resident, 2)
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.AssignmentRow(allocation.ID).Exec(ctx, tx, `UPDATE allocation_assignments SET
			deployment_id = $1, desired_spec_revision = 129, desired_rollout_generation = 129 WHERE id = $2`, historyID(latest), allocation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resident, err = store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertClosure(resident, 1)
	replayed, err := replica.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(resident, replayed) {
		t.Fatalf("journal replay differs from operational closure: %v", err)
	}
	history, err := testDelivery(store).ListServiceDeployments(ctx, testUser("user-1"), service.ID, latest)
	if err != nil || len(history) != latest {
		t.Fatalf("SQL history contains %d entries, want %d: %v", len(history), latest, err)
	}
	if _, err := store.compactJournal(ctx, 0); err != nil {
		t.Fatal(err)
	}
	rebuilt, err = journal.New(store.db, "default", nil).Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(resident, rebuilt) {
		t.Fatalf("compacted SQL rebuild differs: %v", err)
	}
}
