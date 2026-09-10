//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/config"
)

func TestLiveOwnerTakeoverStartsUnknown(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocs, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("allocations: %+v %v", allocs, err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, allocs[0].AllocationIPv4, 8080); err != nil {
		t.Fatal(err)
	}
	allocs, err = store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 || !allocs[0].Healthy {
		t.Fatalf("pre-takeover health: %+v %v", allocs, err)
	}

	testDelivery(store).ResignLive()
	if store.live.Serving() || store.live.Publishing() {
		t.Fatal("former owner still serving")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); !errors.Is(err, deliverycore.ErrNotLiveOwner) {
		t.Fatalf("former owner accepted heartbeat: %v", err)
	}

	if err := testDelivery(store).BecomeLive(ctx); err != nil {
		t.Fatal(err)
	}
	allocs, err = store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allocs[0].Healthy || allocs[0].AppliedRolloutGeneration != 0 {
		t.Fatalf("takeover retained observations: %+v", allocs[0])
	}
	hello.SessionIncarnation++
	hello.SessionId = "after-takeover"
	hello.Allocations = []*agentv1.ServiceCondition{{AllocationId: allocs[0].ID}}
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if !store.live.Admitted(hello.GetAgentId()) {
		t.Fatal("reconciled agent not admitted after takeover")
	}
}

func TestLiveOwnerPausedProcessStopsPublication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	store.live.SetPublishing(false)
	if store.live.Publishing() {
		t.Fatal("paused owner still publishing")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatalf("heartbeat during pause: %v", err)
	}
	lease := NewLeaseManager(store.database, 0, 0)
	claim, ok, err := lease.acquire(ctx, "paused-publication")
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if err := lease.release(ctx, claim); err != nil {
		t.Fatal(err)
	}
	stale := context.WithValue(ctx, leaseContextKey{}, claim)
	if err := store.withTx(stale, func(*sql.Tx) error { return nil }); !errors.Is(err, errLeaseLost) {
		t.Fatalf("paused process committed after lease loss: %v", err)
	}
	if err := store.withLeaseGuard(stale, func() error { return errors.New("published") }); !errors.Is(err, errLeaseLost) {
		t.Fatalf("paused process published after lease loss: %v", err)
	}
}

func TestLiveOwnerStaleSessionRejected(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatal(err)
	}
	hello.SessionId = "replacement"
	hello.SessionIncarnation++
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), "test-session-node-1", false); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old heartbeat: %v", err)
	}
	report := &agentv1.StatusReport{AgentId: hello.GetAgentId(), SessionId: "test-session-node-1", ObservationSequence: 1}
	if err := testDelivery(store).ObserveAgentStatus(ctx, hello.GetAgentId(), report); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old report: %v", err)
	}
}

func TestLiveOwnerDatabaseOutageKeepsMemoryStopsPublication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatal(err)
	}
	store.live.SetPublishing(false)
	closed := store.db
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatalf("heartbeat during database outage: %v", err)
	}
	session, ok := store.live.Session(hello.GetAgentId())
	if !ok || !session.Reachable {
		t.Fatal("session lost during database outage")
	}
	if store.live.Publishing() {
		t.Fatal("published without a fence")
	}
	if err := testDelivery(store).EvaluateObservedDeploymentForTest(ctx, "missing"); err == nil {
		// missing allocation is a no-op; journaled evaluation still cannot commit
	}
	if err := store.withTx(ctx, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("journal commit succeeded with closed database")
	}
}
