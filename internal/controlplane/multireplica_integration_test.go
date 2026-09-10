//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
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

func TestPlatformRevisionCommitsAtomicallyWithStoreTransaction(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	events := NewPlatformEvents(store.events, time.Millisecond)

	before, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform_operators(user_id, created_at) VALUES ('operator-a', statement_timestamp())`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("committed revision = %d, want %d", after, before+1)
	}

	rollbackErr := errors.New("rollback")
	if err := store.withTx(ctx, func(tx *sql.Tx) error {
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
	owner := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	owner.SetAdvertise("owner:9443")
	standby := NewLeaseManager(store.database, time.Minute, time.Millisecond)
	standby.SetAdvertise("standby:9443")

	if _, acquired, err := owner.acquire(ctx, SingletonLeaseName); err != nil || !acquired {
		t.Fatalf("owner acquire = (%v, %v)", acquired, err)
	}
	held, addr, err := owner.Lookup(ctx, SingletonLeaseName)
	if err != nil || !held || addr != "owner:9443" {
		t.Fatalf("owner lookup = held=%t addr=%q err=%v", held, addr, err)
	}
	held, addr, err = standby.Lookup(ctx, SingletonLeaseName)
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
	err = store.withTx(staleCtx, func(*sql.Tx) error { return nil })
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale owner transaction error = %v, want lease lost", err)
	}
	freshCtx := context.WithValue(ctx, leaseContextKey{}, secondClaim)
	if err := store.withTx(freshCtx, func(*sql.Tx) error { return nil }); err != nil {
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
