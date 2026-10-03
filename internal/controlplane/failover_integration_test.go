//go:build integration

package controlplane

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func TestFailoverServicesFromAgentTargetsExpiredNode(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-1", "node-2"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := createService(ctx, store, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "node-1").ID

	staleAt := time.Now().UTC().Add(-2 * deliverycore.AgentHealthyTTL)
	fixtureLive(store).SetLastContactForTest("node-1", staleAt)
	notified, environmentsChanged, err := newTestDelivery(store, nil, nil, nil).failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-deliverycore.AgentHealthyTTL))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 2 {
		t.Fatalf("expected old and new agents to be notified, got %v", notified)
	}
	if len(environmentsChanged) != 1 || environmentsChanged[0] != environmentID {
		t.Fatalf("changed environments = %v, want %s", environmentsChanged, environmentID)
	}
	got, err := store.reads.ServiceByID(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllocatedAgentID != "node-2" {
		t.Fatalf("service assigned to %s, want node-2", got.AllocatedAgentID)
	}
	requireNodeLossReplacement(t, store, service.ID, originalID, "node-1", "node-2")
}

func TestFailoverServicesFromAgentIgnoresFreshNode(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	notified, environmentsChanged, err := newTestDelivery(store, nil, nil, nil).failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-deliverycore.AgentHealthyTTL))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Fatalf("fresh node triggered notifications: %v", notified)
	}
	if len(environmentsChanged) != 0 {
		t.Fatalf("fresh node changed environments: %v", environmentsChanged)
	}
}

func TestFailoverReconcilerFindsPersistedStaleAgent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-stale")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	lastSeen := now.Add(-2 * deliverycore.AgentHealthyTTL)
	fixtureLive(store).SetLastContactForTest("node-stale", lastSeen)

	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := newServiceFailoverReconciler(delivery, time.Second, deliverycore.AgentHealthyTTL)
	_, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO platform_operators(user_id, created_at) VALUES ('user-1', statement_timestamp())`); err != nil {
		t.Fatal(err)
	}
	agents, err := store.reads.ListAgents(ctx, testUser("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "node-stale" || agents[0].LifecycleState != deliverycore.AgentStateUnavailable {
		t.Fatalf("agents after reconcile = %+v, want node-stale unavailable", agents)
	}
	var administrationState string
	if err := store.db.QueryRowContext(ctx, `SELECT lifecycle_state FROM agent_administration WHERE agent_id = 'node-stale'`).Scan(&administrationState); err != nil {
		t.Fatal(err)
	}
	if administrationState != string(deliverycore.AgentStateActive) {
		t.Fatalf("scheduler rewrote operator administration to %q", administrationState)
	}
}

func TestFailoverReconcilerTriggersStatelessServiceRollover(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-a", "node-b"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := createService(ctx, store, "user-1", environmentID, "failover-web", serviceSpec(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	original := mustAllocationOnAgent(t, store, service.ID, "node-a")
	originalID := original.ID
	if err := store.markAllocationHealthyForTest(ctx, service.ID, original.AllocationIPv6, 8080); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	lastSeen := now.Add(-2 * deliverycore.AgentHealthyTTL)
	fixtureLive(store).SetLastContactForTest("node-a", lastSeen)

	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := newServiceFailoverReconciler(delivery, time.Second, deliverycore.AgentHealthyTTL)
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile failover: %v", err)
	}
	if len(result.NotifyAgentIDs) == 0 {
		t.Fatalf("expected cluster notifications after rollover, got %v", result.NotifyAgentIDs)
	}

	replacement := requireNodeLossReplacement(t, store, service.ID, originalID, "node-a", "node-b")
	if replacement.Phase != "Pending" || replacement.Healthy ||
		len(replacement.HealthyIPv4Ports) != 0 || len(replacement.HealthyIPv6Ports) != 0 {
		t.Fatalf("replacement was not reset for the surviving agent: %+v", replacement)
	}
	if replacement.AppliedSpecRevision != 0 || replacement.AppliedRolloutGeneration != 0 {
		t.Fatalf("applied state was not cleared on the replacement: %+v", replacement)
	}
	survivor, err := store.reads.AgentByID(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []struct {
		name           string
		subnet         string
		replaced, lost string
	}{
		{"IPv4", survivor.WorkloadIPv4Subnet, replacement.AllocationIPv4, original.AllocationIPv4},
		{"IPv6", survivor.WorkloadIPv6Subnet, replacement.AllocationIPv6, original.AllocationIPv6},
	} {
		if family.replaced == "" {
			t.Fatalf("replacement has no %s address: %+v", family.name, replacement)
		}
		if family.replaced == family.lost {
			t.Fatalf("replacement reused the lost node's %s address %q", family.name, family.lost)
		}
		prefix, err := netip.ParsePrefix(family.subnet)
		if err != nil {
			t.Fatalf("parse node-b %s subnet %q: %v", family.name, family.subnet, err)
		}
		addr, err := netip.ParseAddr(family.replaced)
		if err != nil {
			t.Fatalf("parse replacement %s address %q: %v", family.name, family.replaced, err)
		}
		if !prefix.Contains(addr) {
			t.Fatalf("replacement %s address %q is outside node-b subnet %q", family.name, family.replaced, family.subnet)
		}
	}

	oldState, err := desiredStateForAgent(ctx, store, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	newState, err := desiredStateForAgent(ctx, store, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(oldState.GetServices()) != 0 {
		t.Fatalf("failed agent still has desired services: %d", len(oldState.GetServices()))
	}
	if len(newState.GetServices()) != 1 || newState.GetServices()[0].GetServiceId() != service.ID {
		t.Fatalf("surviving agent desired services = %v, want service %s", newState.GetServices(), service.ID)
	}
	if newState.GetServices()[0].GetAllocationId() == originalID {
		t.Fatal("surviving agent still has the original allocation identity")
	}
}

func mustAllocationOnAgent(t *testing.T, store *persistence, serviceID, agentID string) deliverycore.AllocationRecord {
	t.Helper()
	for _, alloc := range mustListAllocations(t, store, context.Background(), serviceID) {
		if alloc.AgentID == agentID {
			return alloc
		}
	}
	t.Fatalf("no allocation for service %s on %s", serviceID, agentID)
	return deliverycore.AllocationRecord{}
}

func requireNodeLossReplacement(t *testing.T, store *persistence, serviceID, originalID, deadAgent, liveAgent string) deliverycore.AllocationRecord {
	t.Helper()
	allocs := mustListAllocations(t, store, context.Background(), serviceID)
	var lost, live []deliverycore.AllocationRecord
	for _, alloc := range allocs {
		switch {
		case alloc.ID == originalID:
			if alloc.AgentID != deadAgent || alloc.RolloutState != deliverycore.AllocationRolloutLost || alloc.Phase != "Unavailable" {
				t.Fatalf("original allocation was rewritten instead of marked lost: %+v", alloc)
			}
			lost = append(lost, alloc)
		case alloc.AgentID == liveAgent && alloc.RolloutState != deliverycore.AllocationRolloutLost:
			live = append(live, alloc)
		}
	}
	if len(lost) != 1 {
		t.Fatalf("expected the original allocation to remain lost on %s, got %+v", deadAgent, allocs)
	}
	if len(live) != 1 || live[0].ID == originalID {
		t.Fatalf("expected a new allocation on %s, got %+v", liveAgent, allocs)
	}
	return live[0]
}

type countingFailoverIngress struct {
	requests atomic.Int32
}

func (*countingFailoverIngress) Sync(context.Context) error { return nil }

func (*countingFailoverIngress) Converged(context.Context) (bool, error) {
	return true, nil
}

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

	notifier := newNotifier(store.notifications)
	watches := make(map[string]<-chan struct{})
	for _, id := range []string{"old-node", "new-node", "reserved-node"} {
		ch, stop := notifier.Watch(id)
		defer stop()
		watches[id] = ch
	}
	ingress := &countingFailoverIngress{}
	delivery := newTestDelivery(store, notifier, ingress, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := newServiceFailoverReconciler(delivery, time.Second, 30*time.Second)

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
	if _, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), projectID, "data", 64<<20); err != nil {
		t.Fatal(err)
	}
	volumeService, err := createService(ctx, store, "user-1", projectID, "stateful", directImageServiceSpec("example.test/stateful:1", &platformv1.ServiceRuntime{
		CpuMillis: 10, MemoryMebibytes: 16, Volume: &platformv1.ServiceVolumeMount{VolumeName: "data"},
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
	reconciler := newServiceFailoverReconciler(delivery, time.Second, 30*time.Second)

	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MovedServiceIDs) != 0 || len(result.BlockedServiceIDs) != 2 {
		t.Fatalf("unexpected failover result %+v", result)
	}
	volumeAllocation, err := store.primaryAllocationForTest(ctx, volumeService.ID)
	if err != nil {
		t.Fatal(err)
	}
	if volumeAllocation.AgentID != "old-node" || volumeAllocation.Phase != "Unavailable" || !strings.Contains(volumeAllocation.Message, "replicated storage") {
		t.Fatalf("volume allocation did not surface a pinned-storage reason: %+v", volumeAllocation)
	}
	largeAllocation, err := store.primaryAllocationForTest(ctx, largeService.ID)
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
	reconciler := newServiceFailoverReconciler(delivery, time.Second, 30*time.Second)
	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	allocation, err := store.primaryAllocationForTest(ctx, service.ID)
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

// A node loss must not copy the running deployment over an acknowledged but
// unreleased spec change. Before this was fixed the failover copied the old
// deployment, silently rolling the service back and discarding the update.
func TestServiceFailoverPreservesStagedUpdate(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapFailoverProject(t, store)
	for _, id := range []string{"old-node", "new-node"} {
		hello := agentHello(id)
		hello.CpuMillisCapacity = 1_000
		hello.MemoryMebibytesCapacity = 1_024
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 128, Ports: runtimePortsFromInts([]int32{8080})}
	service, err := createService(ctx, store, "user-1", projectID, "web", directImageServiceSpec("example.test/web:1", runtime), "old-node")
	if err != nil {
		t.Fatal(err)
	}
	original := mustAllocationOnAgent(t, store, service.ID, "old-node")
	if err := store.markAllocationHealthyForTest(ctx, service.ID, original.AllocationIPv6, 8080); err != nil {
		t.Fatal(err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	deploymentsBefore := countServiceDeployments(t, store, service.ID)

	stagedSpec := directImageServiceSpec("example.test/web:2", runtime)
	staged, _, err := updateService(ctx, store, "user-1", service.ID, "", stagedSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !staged.PendingChanges || staged.SpecRevision != service.SpecRevision+1 {
		t.Fatalf("update was not staged: %+v", staged)
	}

	now := time.Now().UTC()
	makeAgentUnhealthy(t, store, "old-node", now.Add(-2*time.Minute))
	delivery := newTestDelivery(store, nil, &countingFailoverIngress{}, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := newServiceFailoverReconciler(delivery, time.Second, 30*time.Second)
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.MovedServiceIDs) != 0 || len(result.BlockedServiceIDs) != 0 {
		t.Fatalf("failover touched a service with a staged update: %+v", result)
	}
	if got := serviceSpecRevision(t, store, service.ID); got != staged.SpecRevision {
		t.Fatalf("failover rolled the staged revision back: got %d want %d", got, staged.SpecRevision)
	}
	if got := countServiceDeployments(t, store, service.ID); got != deploymentsBefore {
		t.Fatalf("failover copied a deployment over the staged update: got %d want %d", got, deploymentsBefore)
	}
	var agentID, rolloutState string
	if err := store.db.QueryRowContext(ctx, `SELECT agent_id, rollout_state FROM allocation_assignments WHERE id = $1`, original.ID).Scan(&agentID, &rolloutState); err != nil {
		t.Fatal(err)
	}
	if agentID != "old-node" || rolloutState == deliverycore.AllocationRolloutLost {
		t.Fatalf("staged-update allocation was rewritten: agent=%s state=%s", agentID, rolloutState)
	}

	// The release path now applies the preserved revision on the healthy node.
	released, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.PendingChanges || released.SpecRevision != staged.SpecRevision {
		t.Fatalf("release did not apply the staged revision: %+v", released)
	}
	target := allocationForGeneration(t, store, service.ID, released.RolloutGeneration)
	if len(target) != 1 || target[0].AgentID != "new-node" || target[0].DesiredSpecRevision != staged.SpecRevision {
		t.Fatalf("release did not place the staged revision on the live node: %+v", target)
	}
	markRolloutAllocationReady(t, store, target[0])
	probe := &rolloutIngressProbe{store: store}
	if err := newRolloutReconciler(newTestDelivery(store, nil, probe, nil), time.Second).Reconcile(ctx); err != nil {
		t.Fatalf("reconcile rollout: %v", err)
	}
	assertServingCount(t, store, service.ID, 1)
}

func countServiceDeployments(t *testing.T, store *persistence, serviceID string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM deployments WHERE service_id = $1`, serviceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func serviceSpecRevision(t *testing.T, store *persistence, serviceID string) int64 {
	t.Helper()
	var revision int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT current_spec_revision FROM services WHERE id = $1`, serviceID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func bootstrapFailoverProject(t *testing.T, store *persistence) string {
	t.Helper()
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{{
		ID: "user-1", Email: "user@example.test", Projects: []string{"demo"},
	}}}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v (%d)", err, len(projects))
	}
	return productionEnvironmentID(t, store, projects[0].ID)
}

func makeAgentUnhealthy(t *testing.T, store *persistence, agentID string, lastSeen time.Time) {
	t.Helper()
	fixtureLive(store).SetLastContactForTest(agentID, lastSeen.UTC())
}
