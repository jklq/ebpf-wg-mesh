//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"

	"github.com/google/uuid"
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
	write := func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET operator_restart_nonce = operator_restart_nonce + 1 WHERE id = $1`, allocation.ID)
		if err != nil {
			return err
		}
		journal.RecordAssignment(ctx, allocation.ID)
		return nil
	}
	if err := store.withTx(commandCtx, func(ctx context.Context, tx *sql.Tx) error {
		if err := write(ctx, tx); err != nil {
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
	receipt, err := restarted.Execute(commandCtx, func(context.Context, *sql.Tx) error {
		t.Error("committed command ran twice")
		return errors.New("duplicate")
	})
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
	err = store.withTx(stale, func(ctx context.Context, tx *sql.Tx) error {
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

// TestJournalRecordingMatchesFullStateDiff enables the recording verifier while
// exercising the production mutation APIs. Every command must record exactly
// the durable rows it changed; a mismatch fails the command.
func TestJournalRecordingMatchesFullStateDiff(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	store.journal.SetVerifyRecordings(true)
	t.Cleanup(func() { store.journal.SetVerifyRecordings(false) })

	project, err := store.catalog.createProject(ctx, "owner", "recording")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	for _, name := range []string{"node-1", "node-2"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"first", "second"} {
		if _, err := createScheduledService(ctx, store, "owner", environmentID, name, directImageServiceSpec("example.test/web:1", nil)); err != nil {
			t.Fatal(err)
		}
	}
	released, _, err := releaseEnvironmentForTest(ctx, store, "owner", environmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) == 0 {
		t.Fatal("no services released")
	}
	serviceID := released[0].ID
	if _, _, err := updateService(ctx, store, "owner", serviceID, "first-renamed", directImageServiceSpec("example.test/web:2", nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scaleService(ctx, store, "owner", serviceID, 2); err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createScheduledVolume(ctx, "owner", environmentID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, "owner", volume.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "owner", "first.example.test", serviceID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := store.routing.DeleteDomainBindingRecord(ctx, "owner", "first.example.test"); err != nil {
		t.Fatal(err)
	}
	if len(released) < 2 {
		t.Fatal("expected two released services")
	}
	secondID := released[1].ID
	if _, err := store.catalog.ensureManagedDomainBinding(ctx, project.ID, "managed.example.test", serviceID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.ensureManagedDomainBinding(ctx, project.ID, "managed.example.test", secondID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "ops", Email: "ops@example.com", Operator: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-2", deliverycore.AgentStateCordoned); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-2", deliverycore.AgentStateActive); err != nil {
		t.Fatal(err)
	}
	fixtureLive(store).SetLastContactForTest("node-1", time.Now().UTC().Add(-2*deliverycore.AgentHealthyTTL))
	if _, _, err := newTestDelivery(store, nil, nil, nil).failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-deliverycore.AgentHealthyTTL)); err != nil {
		t.Fatal(err)
	}
	if err := store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return seedActiveDeploymentTx(ctx, tx, serviceID)
	}); err != nil {
		t.Fatal(err)
	}
	current := currentDeploymentForTest(t, store, ctx, serviceID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, "owner", serviceID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "recording-restart", ""); err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", serviceID); err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, "owner", project.ID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createScheduledService(ctx, store, "owner", staging.ID, "staged", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, "owner", staging.ID); err != nil {
		t.Fatal(err)
	}
}

// TestJournalPayloadIsIndependentOfUnrelatedRows seeds many unrelated durable
// rows and then runs an unrelated one-row command. The command payload must
// carry only the environment row, not the full product state.
func TestJournalPayloadIsIndependentOfUnrelatedRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "payload")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)

	const seeded = 500
	if err := store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		for i := 0; i < seeded; i++ {
			id := uuid.NewString()
			if _, err := tx.ExecContext(ctx, `INSERT INTO services(
				id, environment_id, name, current_spec_revision, current_rollout_generation,
				current_resolved_image, last_successful_commit_sha, latest_build_id,
				desired_replica_count, placement_message, created_at, updated_at
			) VALUES ($1, $2, $3, 1, 0, '', '', '', 1, '', $4, $4)`, id, environmentID, fmt.Sprintf("seeded-%d", i), now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, 1, '{}', $2)`, id, now); err != nil {
				return err
			}
			journal.RecordService(ctx, id)
			journal.RecordRevision(ctx, id, 1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Services) < seeded {
		t.Fatalf("seeded %d services, durable state has %d", seeded, len(before.Services))
	}
	if _, err := store.catalog.renameEnvironment(ctx, "owner", environmentID, "payload-renamed"); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT payload FROM cluster_journal WHERE cluster_id = 'default' AND log_index = $1`, before.LogIndex+1).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var batch journal.Batch
	if err := json.Unmarshal(payload, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Services) != 0 || len(batch.Revisions) != 0 {
		t.Fatalf("unrelated rows leaked into command payload: %d services, %d revisions", len(batch.Services), len(batch.Revisions))
	}
	if len(batch.Environments) != 1 {
		t.Fatalf("expected exactly one environment change, got %d", len(batch.Environments))
	}
	if len(payload) > 2048 {
		t.Fatalf("one-row command payload was %d bytes with %d unrelated rows", len(payload), seeded)
	}
}

// TestJournalConcurrentAppendsStayContiguous appends from two independent
// stores against the same database and verifies the journal stays contiguous
// and every command applies exactly once.
func TestJournalConcurrentAppendsStayContiguous(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := journal.New(store.db, "default", nil)
	second := journal.New(store.db, "default", nil)

	const perStore = 10
	errs := make(chan error, 2)
	run := func(js *journal.Store, prefix string) {
		for i := 0; i < perStore; i++ {
			id := uuid.NewString()
			if _, err := js.Execute(ctx, func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, 'user', NULL, statement_timestamp())`, id, prefix, prefix+"-"+id); err != nil {
					return err
				}
				journal.RecordProject(ctx, id)
				return nil
			}); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}
	go run(first, "first")
	go run(second, "second")
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	head, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.LogIndex != before.LogIndex+2*perStore {
		t.Fatalf("head = %d, want %d", head.LogIndex, before.LogIndex+2*perStore)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND log_index > $1`, before.LogIndex).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2*perStore {
		t.Fatalf("journal entries = %d, want %d", count, 2*perStore)
	}
	var gaps int
	if err := store.db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT log_index, lag(log_index) OVER (ORDER BY log_index) AS prev
			FROM cluster_journal WHERE cluster_id = 'default' AND log_index > $1
		) WHERE prev IS NOT NULL AND log_index <> prev + 1`, before.LogIndex).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if gaps != 0 {
		t.Fatalf("journal has %d gaps", gaps)
	}
}

// TestJournalCompactionBootsFromSnapshot seeds durable state, compacts every
// entry into a snapshot, and verifies a fresh store boots from the snapshot
// without the truncated log.
func TestJournalCompactionBootsFromSnapshot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "compact")
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
	if _, _, err := releaseEnvironmentForTest(ctx, store, "owner", environmentID); err != nil {
		t.Fatal(err)
	}
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.renameEnvironment(ctx, "owner", environmentID, "compacted"); err != nil {
		t.Fatal(err)
	}
	current, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := store.compactJournal(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if watermark != current.LogIndex {
		t.Fatalf("watermark = %d, want head %d", watermark, current.LogIndex)
	}
	var remaining, retained int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND log_index <= $1`, watermark).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND log_index > $1`, watermark).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d entries at or below the watermark survived", remaining)
	}
	// A fresh process has no in-memory prefix. Replaying from index 0 would hit
	// a gap; it must boot from the snapshot.
	restarted := journal.New(store.db, "default", nil)
	replayed, err := restarted.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, replayed) {
		t.Fatalf("snapshot boot differs from live state (before=%d kept=%d)", before.LogIndex, retained)
	}
	if replayed.LogIndex != watermark {
		t.Fatalf("fresh boot index = %d, want watermark %d", replayed.LogIndex, watermark)
	}
}

// TestJournalRetryAfterCompactionReturnsReceipt verifies command-ID
// idempotency survives log truncation: receipts live in
// cluster_journal_receipts, so a retry after retain=0 compaction returns the
// original receipt without re-running the command.
func TestJournalRetryAfterCompactionReturnsReceipt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	commandID := uuid.NewString()
	commandCtx := journal.WithCommandID(ctx, commandID)
	projectID := uuid.NewString()
	receipt, err := store.journal.Execute(commandCtx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, 'owner', 'retained', 'user', NULL, statement_timestamp())`, projectID); err != nil {
			return err
		}
		journal.RecordProject(ctx, projectID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.compactJournal(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var journalRows, receiptRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND command_id = $1`, commandID).Scan(&journalRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal_receipts WHERE cluster_id = 'default' AND command_id = $1`, commandID).Scan(&receiptRows); err != nil {
		t.Fatal(err)
	}
	if journalRows != 0 {
		t.Fatalf("truncated command still in journal (%d rows)", journalRows)
	}
	if receiptRows != 1 {
		t.Fatalf("receipt rows = %d, want 1", receiptRows)
	}
	retried, err := store.journal.Execute(commandCtx, func(context.Context, *sql.Tx) error {
		t.Error("truncated command re-ran after compaction")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.LogIndex != receipt.LogIndex || retried.CommandID != commandID {
		t.Fatalf("retry receipt = %+v, want index %d", retried, receipt.LogIndex)
	}
	if head.LogIndex != receipt.LogIndex {
		t.Fatalf("unexpected head %d for receipt %d", head.LogIndex, receipt.LogIndex)
	}
	var projects int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE id = $1`, projectID).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 1 {
		t.Fatalf("project rows = %d after truncated retry", projects)
	}
}

// TestJournalBehindReplicaReloadsSnapshot covers a replica whose in-memory
// prefix predates the compaction watermark: it must recover by re-reading the
// snapshot path rather than assuming a complete log.
func TestJournalBehindReplicaReloadsSnapshot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "behind")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := createScheduledService(ctx, store, "owner", environmentID, "first", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}

	replica := journal.New(store.db, "default", nil)
	if _, err := replica.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	// The replica now holds a prefix. Advance and compact past it.
	if _, err := store.catalog.renameEnvironment(ctx, "owner", environmentID, "advanced"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.compactJournal(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.renameEnvironment(ctx, "owner", environmentID, "advanced-again"); err != nil {
		t.Fatal(err)
	}
	current, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := replica.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, recovered) {
		t.Fatal("behind replica did not recover from the snapshot path")
	}
}

// TestAffectedAgentFanoutIsScoped proves that a change is applied to the live
// view only for the agents whose desired snapshot can change: service and
// volume changes wake every host of the environment (InternalHosts is
// environment-wide), a reconnect hello does not wake peers, assignment changes
// are cluster-wide, and deployment staging wakes no one.
func TestAffectedAgentFanoutIsScoped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "fanout")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	for _, name := range []string{"node-1", "node-2", "node-3"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(name)); err != nil {
			t.Fatal(err)
		}
	}
	service, err := createService(ctx, store, "owner", environmentID, "web", directImageServiceSpec("example.test/web:1", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	api, err := createService(ctx, store, "owner", environmentID, "api", directImageServiceSpec("example.test/api:1", nil), "node-2")
	if err != nil {
		t.Fatal(err)
	}

	live := store.liveImplementation
	node1, stop1 := live.Watch("node-1")
	defer stop1()
	node2, stop2 := live.Watch("node-2")
	defer stop2()
	node3, stop3 := live.Watch("node-3")
	defer stop3()
	drainWatch(node1)
	drainWatch(node2)
	drainWatch(node3)

	// A new empty environment changes no agent's snapshot.
	if _, err := store.catalog.createEnvironment(ctx, "owner", project.ID, "empty"); err != nil {
		t.Fatal(err)
	}
	assertNoWatch(t, "empty environment creation", node1, node2, node3)

	// InternalHosts includes every serving allocation in the environment, so a
	// rename must wake sibling hosts, not only the service's own agent.
	if _, _, err := updateService(ctx, store, "owner", service.ID, "web-renamed", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}
	assertWatch(t, "service rename on hosting agent", node1)
	assertWatch(t, "service rename on sibling host", node2)
	assertNoWatch(t, "service rename on idle agent", node3)

	if _, err := store.catalog.createScheduledVolume(ctx, "owner", environmentID, "data", 64<<20); err != nil {
		t.Fatal(err)
	}
	assertWatch(t, "volume create on environment host", node1)
	assertWatch(t, "volume create on sibling host", node2)
	assertNoWatch(t, "volume create on idle agent", node3)

	if _, err := store.catalog.ensureManagedDomainBinding(ctx, project.ID, "web.example.test", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	assertWatch(t, "managed domain bind on hosting agent", node1)
	assertNoWatch(t, "managed domain bind on sibling host", node2, node3)

	beforeDomains, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.ensureManagedDomainBinding(ctx, project.ID, "web.example.test", api.ID, 8080); err != nil {
		t.Fatal(err)
	}
	assertWatch(t, "domain reassignment previous host", node1)
	assertWatch(t, "domain reassignment new host", node2)
	assertNoWatch(t, "domain reassignment idle agent", node3)
	afterDomains, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterDomains.Agents["node-1"].DesiredRevision == beforeDomains.Agents["node-1"].DesiredRevision {
		t.Fatal("domain reassignment did not bump previous host")
	}
	if afterDomains.Agents["node-2"].DesiredRevision == beforeDomains.Agents["node-2"].DesiredRevision {
		t.Fatal("domain reassignment did not bump new host")
	}
	if afterDomains.Agents["node-3"].DesiredRevision != beforeDomains.Agents["node-3"].DesiredRevision {
		t.Fatalf("domain reassignment bumped idle desired_revision from %d to %d", beforeDomains.Agents["node-3"].DesiredRevision, afterDomains.Agents["node-3"].DesiredRevision)
	}

	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	after, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Agents["node-2"].DesiredRevision != before.Agents["node-2"].DesiredRevision {
		t.Fatalf("reconnect bumped peer desired_revision from %d to %d", before.Agents["node-2"].DesiredRevision, after.Agents["node-2"].DesiredRevision)
	}
	if after.Agents["node-3"].DesiredRevision != before.Agents["node-3"].DesiredRevision {
		t.Fatalf("reconnect bumped idle desired_revision from %d to %d", before.Agents["node-3"].DesiredRevision, after.Agents["node-3"].DesiredRevision)
	}
	if after.Agents["node-1"].DesiredRevision != before.Agents["node-1"].DesiredRevision {
		t.Fatalf("reconnect bumped self desired_revision from %d to %d", before.Agents["node-1"].DesiredRevision, after.Agents["node-1"].DesiredRevision)
	}
	drainWatch(node1)
	drainWatch(node2)
	drainWatch(node3)

	// A deployment-only change never appears in a desired snapshot.
	if err := store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var deploymentID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM deployments WHERE service_id = $1 AND is_current LIMIT 1`, service.ID).Scan(&deploymentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deployments SET detail = 'staged for test' WHERE id = $1`, deploymentID); err != nil {
			return err
		}
		journal.RecordDeployment(ctx, deploymentID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertNoWatch(t, "deployment staging", node1, node2, node3)

	// Assignment changes are part of the cluster-wide workload identity catalog,
	// so every agent must rebuild its snapshot.
	allocs, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("list allocations: %v", err)
	}
	if err := store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET operator_restart_nonce = operator_restart_nonce + 1 WHERE id = $1`, allocs[0].ID); err != nil {
			return err
		}
		journal.RecordAssignment(ctx, allocs[0].ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertWatch(t, "assignment change on hosting agent", node1)
	assertWatch(t, "assignment change on sibling host", node2)
	assertWatch(t, "assignment change on idle peer", node3)
}

func drainWatch(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func assertWatch(t *testing.T, context string, channels ...<-chan struct{}) {
	t.Helper()
	for _, ch := range channels {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: expected wakeup", context)
		}
	}
}

func assertNoWatch(t *testing.T, context string, channels ...<-chan struct{}) {
	t.Helper()
	for _, ch := range channels {
		select {
		case <-ch:
			t.Fatalf("%s: unexpected wakeup", context)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
