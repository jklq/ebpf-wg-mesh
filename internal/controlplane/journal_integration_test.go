//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestJournalReleaseBatchAndReplay(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "journal")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := createScheduledService(ctx, store, "owner", environmentID, name, directImageServiceSpec("example.test/web:1", nil)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released, err := testDelivery(store).ReleaseEnvironment(contextWithDelegatedUser("owner", ""), environmentID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live.LogIndex != before.LogIndex+1 || len(released) != 2 || len(live.Assignments) != 2 {
		t.Fatalf("release was not one batch: %d -> %d, %d releases, %d assignments", before.LogIndex, live.LogIndex, len(released), len(live.Assignments))
	}
	var payload []byte
	var epoch int64
	if err := store.db.QueryRowContext(ctx, `SELECT payload, authorizing_epoch FROM cluster_journal WHERE cluster_id = 'default' AND log_index = $1`, live.LogIndex).Scan(&payload, &epoch); err != nil {
		t.Fatal(err)
	}
	var batch journal.Batch
	if err := json.Unmarshal(payload, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Assignments) != 2 || len(batch.Services) != 2 || epoch != 1 {
		t.Fatalf("incomplete release command: %+v, epoch %d", batch, epoch)
	}
	for _, result := range released {
		assignment := result.Allocations[0]
		recorded := live.Assignments[assignment.ID]
		if recorded.AgentID != assignment.AgentID || recorded.AllocationIPv4 != assignment.AllocationIPv4 || recorded.AllocationIPv6 != assignment.AllocationIPv6 {
			t.Fatalf("published assignment differs from committed assignment: %+v", recorded)
		}
	}
	// A new process has no prior in-memory state, and may have a different
	// authority epoch. Neither condition changes the meaning of committed entries.
	if err := store.advanceAgentAuthority(ctx, 1); err != nil {
		t.Fatal(err)
	}
	restarted := journal.New(store.db, "default", nil)
	replayed, err := restarted.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, replayed) {
		t.Fatal("replay differs from live durable state")
	}
}

func TestJournalRetryAndAbortDoNotDuplicateAssignments(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	allocation := allocationForGeneration(t, store, service.ID, 1)[0]
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aborted := errors.New("crash before commit")
	id := "assignment-command"
	commandCtx := journal.WithCommandID(ctx, id)
	write := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET operator_restart_nonce = operator_restart_nonce + 1 WHERE id = $1`, allocation.ID)
		return err
	}
	if err := store.withTx(commandCtx, func(tx *sql.Tx) error {
		if err := write(tx); err != nil {
			return err
		}
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatalf("abort result: %v", err)
	}
	rolledBack, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, rolledBack) {
		t.Fatal("aborted transaction changed durable state")
	}
	if err := store.withTx(commandCtx, write); err != nil {
		t.Fatal(err)
	}
	committed, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Assignments[allocation.ID].OperatorRestartNonce != before.Assignments[allocation.ID].OperatorRestartNonce+1 {
		t.Fatal("committed command not applied")
	}
	// This also models a retry after losing the commit response and restarting.
	restarted := journal.New(store.db, "default", nil)
	receipt, err := restarted.Execute(commandCtx, func(*sql.Tx) error { t.Error("committed command ran twice"); return errors.New("duplicate") })
	if err != nil {
		t.Fatal(err)
	}
	if receipt.LogIndex != committed.LogIndex {
		t.Fatal("retry allocated another journal index")
	}
	replayed, err := restarted.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(committed, replayed) {
		t.Fatal("retry changed assignments")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND command_id = $1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("command receipts = %d: %v", count, err)
	}
}

func TestJournalLeaseLossRollsBackDecision(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease := NewLeaseManager(store.database, 0, 0)
	claim, ok, err := lease.acquire(ctx, "journal-test")
	if err != nil || !ok {
		t.Fatalf("acquire: %v, %v", ok, err)
	}
	if err := lease.release(ctx, claim); err != nil {
		t.Fatal(err)
	}
	stale := context.WithValue(ctx, leaseContextKey{}, claim)
	err = store.withTx(stale, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET intent_message = 'stale decision' WHERE service_id = $1`, service.ID)
		return err
	})
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale lease accepted: %v", err)
	}
	after, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale decision changed committed prefix")
	}
}

func TestJournalExcludesTransientHeartbeat(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatal(err)
	}
	after, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("transient heartbeat changed durable journal state")
	}
}
