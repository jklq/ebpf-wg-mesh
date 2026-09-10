//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type countingFailoverIngress struct {
	requests atomic.Int32
}

func (*countingFailoverIngress) Sync(context.Context) error { return nil }

func (i *countingFailoverIngress) RequestSync() {
	i.requests.Add(1)
}

func TestServiceFailoverMovesStatelessServiceAndNotifiesCluster(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapFailoverProject(t, store)

	for _, id := range []string{"old-node", "new-node", "reserved-node"} {
		hello := agentHello(id)
		hello.CpuMillisCapacity = 1_000
		hello.MemoryMebibytesCapacity = 1_024
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatal(err)
		}
	}
	store.reserveAgents("reserved-node")
	service, err := createService(ctx, store, "user-1", projectID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{
		CpuMillis: 100, MemoryMebibytes: 128, Ports: runtimePortsFromInts([]int32{8080}),
	}), "old-node")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "old-node").ID
	if err := store.markAllocationHealthyForTest(ctx, service.ID, mustAllocationOnAgent(t, store, service.ID, "old-node").AllocationIPv6, 8080); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeAgentUnhealthy(t, store, "old-node", now.Add(-2*time.Minute))

	notifier := NewNotifier(store.notifications)
	watches := make(map[string]<-chan struct{})
	for _, id := range []string{"old-node", "new-node", "reserved-node"} {
		ch, stop := notifier.Watch(id)
		defer stop()
		watches[id] = ch
	}
	ingress := &countingFailoverIngress{}
	delivery := newTestDelivery(store, notifier, ingress, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := NewServiceFailoverReconciler(delivery, time.Second, 30*time.Second)

	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.MovedServiceIDs) != 1 || result.MovedServiceIDs[0] != service.ID {
		t.Fatalf("unexpected moved services %#v", result.MovedServiceIDs)
	}
	for id, ch := range watches {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("agent %s was not notified", id)
		}
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected one ingress resync, got %d", got)
	}

	allocation := requireNodeLossReplacement(t, store, service.ID, originalID, "old-node", "new-node")
	if allocation.Phase != "Pending" || allocation.Healthy || allocation.AllocationIPv4 == "" || allocation.AllocationIPv6 == "" ||
		len(allocation.HealthyIPv4Ports) != 0 || len(allocation.HealthyIPv6Ports) != 0 {
		t.Fatalf("replacement was not reset after failover: %+v", allocation)
	}
	if allocation.AppliedSpecRevision != 0 || allocation.AppliedRolloutGeneration != 0 {
		t.Fatalf("applied replacement state was not reset: %+v", allocation)
	}
	oldState, err := desiredStateForAgent(ctx, store, "old-node")
	if err != nil {
		t.Fatal(err)
	}
	newState, err := desiredStateForAgent(ctx, store, "new-node")
	if err != nil {
		t.Fatal(err)
	}
	if len(oldState.GetServices()) != 0 || len(newState.GetServices()) != 1 || newState.GetServices()[0].GetServiceId() != service.ID {
		t.Fatalf("unexpected old/new desired state: old=%d new=%d", len(oldState.GetServices()), len(newState.GetServices()))
	}

	second, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.MovedServiceIDs) != 0 || len(second.BlockedServiceIDs) != 0 || ingress.requests.Load() != 1 {
		t.Fatalf("second reconcile was not idempotent: %+v ingress=%d", second, ingress.requests.Load())
	}
}

func TestServiceFailoverSurfacesVolumeAndCapacityBlocks(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapFailoverProject(t, store)

	old := agentHello("old-node")
	old.CpuMillisCapacity = 1_000
	old.MemoryMebibytesCapacity = 1_024
	if _, err := upsertTestAgent(t, store, ctx, old); err != nil {
		t.Fatal(err)
	}
	target := agentHello("small-node")
	target.CpuMillisCapacity = 50
	target.MemoryMebibytesCapacity = 64
	if _, err := upsertTestAgent(t, store, ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createScheduledVolume(ctx, "user-1", projectID, "data", 64<<20); err != nil {
		t.Fatal(err)
	}
	volumeService, err := createService(ctx, store, "user-1", projectID, "stateful", directImageServiceSpec("example.test/stateful:1", &platformv1.ServiceRuntime{
		CpuMillis: 10, MemoryMebibytes: 16, VolumeName: "data",
	}), "old-node")
	if err != nil {
		t.Fatal(err)
	}
	largeService, err := createService(ctx, store, "user-1", projectID, "large", directImageServiceSpec("example.test/large:1", &platformv1.ServiceRuntime{
		CpuMillis: 100, MemoryMebibytes: 128,
	}), "old-node")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeAgentUnhealthy(t, store, "old-node", now.Add(-2*time.Minute))
	ingress := &countingFailoverIngress{}
	delivery := newTestDelivery(store, nil, ingress, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := NewServiceFailoverReconciler(delivery, time.Second, 30*time.Second)

	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MovedServiceIDs) != 0 || len(result.BlockedServiceIDs) != 2 {
		t.Fatalf("unexpected failover result %+v", result)
	}
	volumeAllocation, err := store.reads.allocationByServiceID(ctx, volumeService.ID)
	if err != nil {
		t.Fatal(err)
	}
	if volumeAllocation.AgentID != "old-node" || volumeAllocation.Phase != "Unavailable" || !strings.Contains(volumeAllocation.Message, "replicated storage") {
		t.Fatalf("volume allocation did not surface a pinned-storage reason: %+v", volumeAllocation)
	}
	largeAllocation, err := store.reads.allocationByServiceID(ctx, largeService.ID)
	if err != nil {
		t.Fatal(err)
	}
	if largeAllocation.AgentID != "old-node" || largeAllocation.Phase != "Unavailable" || !strings.Contains(largeAllocation.Message, "blocked") || !(strings.Contains(largeAllocation.Message, "capacity") || strings.Contains(largeAllocation.Message, "CPU") || strings.Contains(largeAllocation.Message, "memory")) {
		t.Fatalf("capacity allocation did not surface a no-capacity reason: %+v", largeAllocation)
	}
	if ingress.requests.Load() != 1 {
		t.Fatalf("expected one coalesced ingress request, got %d", ingress.requests.Load())
	}
	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if ingress.requests.Load() != 1 {
		t.Fatalf("idempotent blocked reconcile requested ingress again: %d", ingress.requests.Load())
	}
}

func TestServiceFailoverKeepsManagedWorkloadOnTrustedAgent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	trusted := agentHello("trusted-node")
	pool := agentHello("pool-node")
	if _, err := upsertTestAgent(t, store, ctx, trusted); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, pool); err != nil {
		t.Fatal(err)
	}
	store.reserveAgents(trusted.AgentId)
	project, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	service, _, err := newTestDelivery(store, nil, nil, nil).EnsureManagedService(ctx, project.ID, "dashboard", directImageServiceSpec("example.test/dashboard:1", nil), trusted.AgentId)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeAgentUnhealthy(t, store, trusted.AgentId, now.Add(-2*time.Minute))
	delivery := newTestDelivery(store, nil, &countingFailoverIngress{}, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := NewServiceFailoverReconciler(delivery, time.Second, 30*time.Second)
	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	allocation, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AgentID != trusted.AgentId || allocation.Phase != "Unavailable" || !strings.Contains(allocation.Message, "trusted") {
		t.Fatalf("managed allocation migrated or lacked a trust failure: %+v", allocation)
	}
}

func TestConcurrentServiceFailoverMovesOnlyOnce(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapFailoverProject(t, store)
	for _, id := range []string{"old-node", "new-node"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(id)); err != nil {
			t.Fatal(err)
		}
	}
	service, err := createService(ctx, store, "user-1", projectID, "web", directImageServiceSpec("example.test/web:1", nil), "old-node")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "old-node").ID
	now := time.Now().UTC()
	makeAgentUnhealthy(t, store, "old-node", now.Add(-2*time.Minute))
	before, err := store.currentDesiredRevisionForAgent(ctx, "new-node")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan deliverycore.ServiceFailoverResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := testDelivery(store).failoverUnhealthyServices(ctx, now, 30*time.Second)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reconcile: %v", err)
		}
	}
	moves := 0
	for result := range results {
		moves += len(result.MovedServiceIDs)
	}
	if moves != 1 {
		t.Fatalf("expected exactly one move, got %d", moves)
	}
	after, err := store.currentDesiredRevisionForAgent(ctx, "new-node")
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("expected one desired revision bump, before=%d after=%d", before, after)
	}
	_ = requireNodeLossReplacement(t, store, service.ID, originalID, "old-node", "new-node")
}

func bootstrapFailoverProject(t *testing.T, store *persistence) string {
	t.Helper()
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{{
		ID: "user-1", Email: "user@example.test", Projects: []string{"demo"},
	}}}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v (%d)", err, len(projects))
	}
	return productionEnvironmentID(t, store, projects[0].ID)
}

func makeAgentUnhealthy(t *testing.T, store *persistence, agentID string, lastSeen time.Time) {
	t.Helper()
	fixtureLive(store).SetLastContactForTest(agentID, lastSeen.UTC())
}
