//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestLiveWatchFiresAfterDurableApply(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := upsertTestAgent(t, store, ctx, &agentv1.AgentHello{AgentId: "agent-a", Name: "agent-a"}); err != nil {
		t.Fatal(err)
	}
	wake, stop := NewNotifier(store.notifications).Watch("agent-a")
	defer stop()
	bumpDesiredRevisionsForTest(t, store, ctx, []string{"agent-a"})
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("live watch did not fire after durable apply")
	}
}

func TestReplicaSnapshotAppliesDurableIntoLive(t *testing.T) {
	ctx := context.Background()
	storeA := openTestStore(t)
	if _, err := upsertTestAgent(t, storeA, ctx, &agentv1.AgentHello{AgentId: "agent-a", Name: "agent-a"}); err != nil {
		t.Fatal(err)
	}
	storeB, err := openPersistence(config.DatabaseConfig{
		URL: sharedTestDatabase(t), MaxOpenConns: 2, MaxIdleConns: 2,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storeB.Close() })
	wake, stop := NewNotifier(storeB.notifications).Watch("agent-a")
	defer stop()
	bumpDesiredRevisionsForTest(t, storeA, ctx, []string{"agent-a"})
	snapshot, err := storeB.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	position := newDelivery(storeB, nil, nil, nil, nil).LivePosition()
	if position.AcceptedDurable != snapshot.LogIndex {
		t.Fatalf("live durable position = %d, journal = %d", position.AcceptedDurable, snapshot.LogIndex)
	}
	if position.Ready || storeB.routing.live.Publishing() {
		t.Fatal("replaying another replica's journal must not acquire live ownership")
	}
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("replica B live view did not apply replica A's journal prefix")
	}
}

func TestCoordinationTransactionDoesNotAdvancePlatformRevision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	events := NewPlatformEvents(store.events, time.Millisecond)

	before, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform_operators(user_id, created_at) VALUES ('operator-a', statement_timestamp())`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("coordination transaction advanced revision from %d to %d", before, after)
	}

	rollbackErr := errors.New("rollback")
	if err := store.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO platform_operators(user_id, created_at) VALUES ('operator-b', statement_timestamp())`); err != nil {
			return err
		}
		return rollbackErr
	}); !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback error = %v", err)
	}
	unchanged, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != after {
		t.Fatalf("rolled-back transaction advanced revision from %d to %d", after, unchanged)
	}
}

func TestLeaseManagerRecordsAdvertiseAddrForRedirect(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const leaseName = "advertise-addr-test"
	owner := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	owner.SetAdvertise("owner:9443")
	standby := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	standby.SetAdvertise("standby:9443")

	if _, acquired, err := owner.acquire(ctx, leaseName); err != nil || !acquired {
		t.Fatalf("owner acquire = (%v, %v)", acquired, err)
	}
	held, addr, err := owner.Lookup(ctx, leaseName)
	if err != nil || !held || addr != "owner:9443" {
		t.Fatalf("owner lookup = held=%t addr=%q err=%v", held, addr, err)
	}
	held, addr, err = standby.Lookup(ctx, leaseName)
	if err != nil || held || addr != "owner:9443" {
		t.Fatalf("standby lookup = held=%t addr=%q err=%v", held, addr, err)
	}
}

func TestLeaseTakeoverFencesFormerOwner(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	second := NewLeaseManager(store.database, time.Minute, time.Millisecond)

	firstClaim, acquired, err := first.acquire(ctx, "singleton-test")
	if err != nil || !acquired {
		t.Fatalf("first acquire = (%v, %v)", acquired, err)
	}
	if _, acquired, err := second.acquire(ctx, "singleton-test"); err != nil || acquired {
		t.Fatalf("concurrent acquire = (%v, %v), want false, nil", acquired, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE control_plane_leases SET expires_at = statement_timestamp() WHERE name = 'singleton-test'`); err != nil {
		t.Fatal(err)
	}
	secondClaim, acquired, err := second.acquire(ctx, "singleton-test")
	if err != nil || !acquired {
		t.Fatalf("takeover acquire = (%v, %v)", acquired, err)
	}
	if secondClaim.token <= firstClaim.token {
		t.Fatalf("takeover token = %d, want greater than %d", secondClaim.token, firstClaim.token)
	}

	staleCtx := context.WithValue(ctx, leaseContextKey{}, firstClaim)
	err = store.withProductTx(staleCtx, func(context.Context, *sql.Tx) error { return nil })
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale owner transaction error = %v, want lease lost", err)
	}
	freshCtx := context.WithValue(ctx, leaseContextKey{}, secondClaim)
	if err := store.withProductTx(freshCtx, func(context.Context, *sql.Tx) error { return nil }); err != nil {
		t.Fatalf("new owner transaction: %v", err)
	}
}

func TestLeaseGuardSerializesExternalEffectWithTakeover(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	second := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	claim, acquired, err := first.acquire(ctx, "external-effect-test")
	if err != nil || !acquired {
		t.Fatalf("first acquire = (%v, %v)", acquired, err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	guardDone := make(chan error, 1)
	guardCtx := context.WithValue(ctx, leaseContextKey{}, claim)
	go func() {
		guardDone <- store.withLeaseGuard(guardCtx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	takeoverDone := make(chan error, 1)
	go func() {
		if _, err := store.db.ExecContext(ctx, `UPDATE control_plane_leases SET expires_at = statement_timestamp() WHERE name = 'external-effect-test'`); err != nil {
			takeoverDone <- err
			return
		}
		_, acquired, err := second.acquire(ctx, "external-effect-test")
		if err == nil && !acquired {
			err = errors.New("successor did not acquire expired lease")
		}
		takeoverDone <- err
	}()

	select {
	case err := <-takeoverDone:
		t.Fatalf("takeover completed while external effect was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-guardDone; err != nil {
		t.Fatalf("lease guard: %v", err)
	}
	select {
	case err := <-takeoverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("takeover did not complete after external effect finished")
	}
}

func TestHeldLeaseRenewsUntilReleased(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first := NewLeaseManager(store.database, 300*time.Millisecond, 5*time.Millisecond)
	second := NewLeaseManager(store.database, 300*time.Millisecond, 5*time.Millisecond)
	leaseCtx, release, err := first.hold(ctx, "held-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	time.Sleep(700 * time.Millisecond)
	if err := leaseCtx.Err(); err != nil {
		t.Fatalf("held lease was lost: %v", err)
	}
	if _, acquired, err := second.acquire(ctx, "held-test"); err != nil || acquired {
		t.Fatalf("acquire while held = (%v, %v), want false, nil", acquired, err)
	}
	release()
	if _, acquired, err := second.acquire(ctx, "held-test"); err != nil || !acquired {
		t.Fatalf("acquire after release = (%v, %v), want true, nil", acquired, err)
	}
}

func TestLeaseManagerSelectsOneReplicaAndHandsOffOnShutdown(t *testing.T) {
	store := openTestStore(t)
	first := NewLeaseManager(store.database, time.Second, 10*time.Millisecond)
	second := NewLeaseManager(store.database, time.Second, 10*time.Millisecond)
	firstCtx, stopFirst := context.WithCancel(context.Background())
	secondCtx, stopSecond := context.WithCancel(context.Background())
	defer stopFirst()
	defer stopSecond()

	started := make(chan string, 2)
	job := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			started <- name
			<-ctx.Done()
			return nil
		}
	}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- first.Run(firstCtx, "handoff-test", job("first")) }()
	go func() { secondDone <- second.Run(secondCtx, "handoff-test", job("second")) }()

	var winner string
	select {
	case winner = <-started:
	case <-time.After(time.Second):
		t.Fatal("neither replica acquired the singleton lease")
	}
	select {
	case duplicate := <-started:
		t.Fatalf("replicas ran concurrently: %s and %s", winner, duplicate)
	case <-time.After(100 * time.Millisecond):
	}
	if winner == "first" {
		stopFirst()
	} else {
		stopSecond()
	}
	select {
	case successor := <-started:
		if successor == winner {
			t.Fatalf("same stopped replica reacquired lease: %s", successor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("standby replica did not take over")
	}
	stopFirst()
	stopSecond()
	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s lease manager: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s lease manager did not stop", name)
		}
	}
}

func TestSharedStorageBindingRejectsReplicaLocalDirectory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	shared := t.TempDir()
	if err := verifySharedControlPlaneDirectory(ctx, store, "state-test", shared); err != nil {
		t.Fatal(err)
	}
	if err := verifySharedControlPlaneDirectory(ctx, store, "state-test", shared); err != nil {
		t.Fatalf("same shared directory was rejected: %v", err)
	}
	if err := verifySharedControlPlaneDirectory(ctx, store, "state-test", t.TempDir()); err == nil {
		t.Fatal("replica-local directory was accepted for registered shared storage")
	}
}

func TestNonOwnerReplicaDoesNotServeOwnerLocalAllocations(t *testing.T) {
	ctx := context.Background()
	owner := openTestStore(t)

	// The second replica is opened before the owner writes anything, so its live
	// view stays empty and cannot accidentally satisfy the read from a snapshot.
	nonOwner, err := openPersistence(config.DatabaseConfig{
		URL: sharedTestDatabase(t), MaxOpenConns: 2, MaxIdleConns: 2,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nonOwner.Close() })
	newDelivery(nonOwner, nil, nil, nil, nil)
	if nonOwner.fleet.sessions.Serving() {
		t.Fatal("second replica unexpectedly became the live owner")
	}

	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, owner, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := owner.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := owner.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	environmentID := productionEnvironmentID(t, owner, projects[0].ID)
	service, err := createScheduledService(ctx, owner, "user-1", environmentID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, owner, "user-1", environmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	ownerAllocations, err := owner.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(ownerAllocations) == 0 {
		t.Fatalf("owner allocations: %+v %v", ownerAllocations, err)
	}
	if err := reportActiveForTest(ctx, owner, service.ID); err != nil {
		t.Fatalf("record owner observation: %v", err)
	}

	_, allocations, err := nonOwner.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
	if err == nil && len(allocations) == 0 {
		t.Fatal("non-owner replica silently returned zero allocations for a released service")
	}
	if err != nil && !errors.Is(err, deliverycore.ErrNotLiveOwner) {
		t.Fatalf("non-owner replica service status error = %v, want ErrNotLiveOwner or durable allocations", err)
	}
}

func TestReplicaRecoversAfterJournalCompaction(t *testing.T) {
	ctx := context.Background()
	storeA := openTestStore(t)
	if _, err := upsertTestAgent(t, storeA, ctx, &agentv1.AgentHello{AgentId: "agent-a", Name: "agent-a"}); err != nil {
		t.Fatal(err)
	}
	project, err := storeA.catalog.createProject(ctx, testUser("owner"), "replica-compact")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, storeA, project.ID)
	if _, err := createScheduledService(ctx, storeA, "owner", environmentID, "first", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}

	storeB, err := openPersistence(config.DatabaseConfig{
		URL: sharedTestDatabase(t), MaxOpenConns: 2, MaxIdleConns: 2,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storeB.Close() })
	if _, err := storeB.journal.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := storeA.catalog.renameEnvironment(ctx, testUser("owner"), environmentID, "compacted"); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.compactJournal(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.catalog.renameEnvironment(ctx, testUser("owner"), environmentID, "compacted-again"); err != nil {
		t.Fatal(err)
	}

	current, err := storeA.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := storeB.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, recovered) {
		t.Fatalf("standby replica diverged after compaction: replica=%d owner=%d diff=%+v", recovered.LogIndex, current.LogIndex, journal.Diff(recovered, current))
	}
	if recovered.Environments[environmentID].Name != "compacted-again" {
		t.Fatalf("standby did not observe post-compaction change: %+v", recovered.Environments[environmentID])
	}
}
