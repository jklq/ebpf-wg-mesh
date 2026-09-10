//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/routing"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestReplicaPlacementIsDeterministicAndAvoidsColocation(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b", "node-c"})

	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 3)
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 3 {
		t.Fatalf("expected 3 allocations, got %d", len(allocations))
	}
	agents := allocationAgentIDs(allocations)
	if agents["node-a"] != 1 || agents["node-b"] != 1 || agents["node-c"] != 1 {
		t.Fatalf("expected one replica per agent, got %v", agents)
	}

	service2, err := createService(ctx, store, "user-1", envID, "api", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService(2): %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service2.ID, 3)
	second := allocationAgentIDs(mustListAllocations(t, store, ctx, service2.ID))
	if second["node-a"] != 1 || second["node-b"] != 1 || second["node-c"] != 1 {
		t.Fatalf("placement was not deterministic, got %v", second)
	}
}

func TestReplicaPlacementColocatesOnlyWhenCapacityRequiresIt(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	hello := agentHello("node-b")
	hello.CpuMillisCapacity = 50
	hello.MemoryMebibytesCapacity = 32
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)
	agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID))
	if agents["node-a"] != 2 || agents["node-b"] != 0 {
		t.Fatalf("expected both replicas on node-a when node-b lacks capacity, got %v", agents)
	}
}

func TestReplicaScaleExplainsPendingCapacityFailures(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	hello := agentHello("node-a")
	hello.CpuMillisCapacity = 250
	hello.MemoryMebibytesCapacity = 256
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	queued, allocations, err := scaleService(ctx, store, "user-1", service.ID, 4)
	if err != nil {
		t.Fatalf("scaleService: %v", err)
	}
	if queued.DesiredReplicaCount != 1 {
		t.Fatalf("live desired replica count = %d, want 1 until deploy", queued.DesiredReplicaCount)
	}
	if queued.Spec.GetDesiredReplicaCount() != 4 {
		t.Fatalf("queued replica count = %d, want 4", queued.Spec.GetDesiredReplicaCount())
	}
	if len(allocations) != 1 {
		t.Fatalf("expected live allocations to stay at 1 until deploy, got %d", len(allocations))
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	scaled, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	allocations = mustListAllocations(t, store, ctx, service.ID)
	if scaled.DesiredReplicaCount != 4 {
		t.Fatalf("desired replica count = %d, want 4", scaled.DesiredReplicaCount)
	}
	if len(allocations) != 2 {
		t.Fatalf("expected 2 placed replicas, got %d", len(allocations))
	}
	if !strings.Contains(scaled.PlacementMessage, "2 of 4 replicas placed") {
		t.Fatalf("expected pending capacity message, got %q", scaled.PlacementMessage)
	}
}

func TestVolumeAndReplicasAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	if _, err := store.catalog.createScheduledVolume(ctx, "user-1", envID, "data", 64<<20); err != nil {
		t.Fatalf("createScheduledVolume: %v", err)
	}

	volumeSpec := replicaSpec(100, 64)
	volumeSpec.Runtime.VolumeName = "data"
	volumeService, err := createService(ctx, store, "user-1", envID, "disk", volumeSpec, "node-a")
	if err != nil {
		t.Fatalf("createService(volume): %v", err)
	}
	if _, _, err := scaleService(ctx, store, "user-1", volumeService.ID, 2); !errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) {
		t.Fatalf("scale volume service to 2: got %v", err)
	}

	replicaService, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService(replicas): %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, replicaService.ID, 2)
	volumeSpec.Runtime.CpuMillis = replicaService.Spec.GetRuntime().GetCpuMillis()
	volumeSpec.Runtime.MemoryMebibytes = replicaService.Spec.GetRuntime().GetMemoryMebibytes()
	volumeSpec.Runtime.Ports = replicaService.Spec.GetRuntime().GetPorts()
	if _, _, err := updateService(ctx, store, "user-1", replicaService.ID, replicaService.Name, volumeSpec); !errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) {
		t.Fatalf("attach volume to replicated service: got %v", err)
	}
}

func TestReplicaScaleRejectsZero(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := scaleService(ctx, store, "user-1", service.ID, 0); !errors.Is(err, deliverycore.ErrInvalidReplicaCount) {
		t.Fatalf("expected errInvalidReplicaCount, got %v", err)
	}
}

func TestReplicaDesiredStateIsAgentLocalAndIdentityCatalogIsClusterWide(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)

	stateA, err := desiredStateForAgent(ctx, store, "node-a")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-a): %v", err)
	}
	stateB, err := desiredStateForAgent(ctx, store, "node-b")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-b): %v", err)
	}
	if len(stateA.GetServices()) != 1 || stateA.GetServices()[0].GetAllocationId() == "" {
		t.Fatalf("node-a desired services = %+v", stateA.GetServices())
	}
	if len(stateB.GetServices()) != 1 || stateB.GetServices()[0].GetAllocationId() == stateA.GetServices()[0].GetAllocationId() {
		t.Fatalf("expected independent allocation identities, a=%q b=%+v", stateA.GetServices()[0].GetAllocationId(), stateB.GetServices())
	}
	if stateA.GetServices()[0].GetPrivateIpv6() == stateB.GetServices()[0].GetPrivateIpv6() {
		t.Fatal("replicas shared a private address")
	}
	if got := len(stateA.GetNodeConfig().GetWorkloadIdentities()); got != 2 {
		t.Fatalf("identity catalog on node-a has %d entries, want 2", got)
	}
	if got := len(stateB.GetNodeConfig().GetWorkloadIdentities()); got != 2 {
		t.Fatalf("identity catalog on node-b has %d entries, want 2", got)
	}
}

func TestReplicaIngressAndInternalDNSPublishOnlyReadyAllocations(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "web.example.com", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if err := store.markAllocationIDHealthyForTest(ctx, allocations[0].ID, "fd00:1::10", 8080); err != nil {
		t.Fatal(err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileRollouts(ctx); err != nil {
		t.Fatalf("promote ready replica: %v", err)
	}

	backends, err := store.routing.HealthyIngressBackends(ctx)
	if err != nil {
		t.Fatalf("listHealthyIngressBackends: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("expected only the ready replica in ingress, got %+v", backends)
	}
	syncer := routing.NewIngressSyncer("http://127.0.0.1:2019/load", store.routing)
	cfg, err := syncer.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Apps.HTTP.Servers["srv0"].Routes[0].Handle[0].Upstreams); got != 1 {
		t.Fatalf("expected 1 ingress upstream, got %d", got)
	}

	if err := store.markAllocationIDHealthyForTest(ctx, allocations[1].ID, "fd00:1::11", 8080); err != nil {
		t.Fatal(err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileRollouts(ctx); err != nil {
		t.Fatalf("promote second ready replica: %v", err)
	}
	cfg, err = syncer.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Apps.HTTP.Servers["srv0"].Routes[0].Handle[0].Upstreams); got != 2 {
		t.Fatalf("expected traffic to be distributed across 2 ready replicas, got %d", got)
	}

	state, err := desiredStateForAgent(ctx, store, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	hosts := state.GetServices()[0].GetInternalHosts()
	if len(hosts) != 2 {
		t.Fatalf("expected internal DNS to publish both ready replicas, got %+v", hosts)
	}
	if hosts[0].GetHostname() != hosts[1].GetHostname() {
		t.Fatalf("internal hostname should stay stable, got %q and %q", hosts[0].GetHostname(), hosts[1].GetHostname())
	}
	if hosts[0].GetIpv6() == hosts[1].GetIpv6() {
		t.Fatal("ready replicas should advertise distinct internal addresses")
	}
}

func TestReplicaFailoverAvoidsColocationAfterNodeLoss(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b", "node-c"})
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)

	past := time.Now().UTC().Add(-time.Minute)
	fixtureLive(store).SetLastContactForTest("node-b", past)
	result, err := testDelivery(store).failoverUnhealthyServices(ctx, time.Now().UTC(), 30*time.Second)
	if err != nil {
		t.Fatalf("failoverUnhealthyServices: %v", err)
	}
	if len(result.MovedServiceIDs) == 0 {
		t.Fatalf("expected the lost replica to be rescheduled, got %+v", result)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	activeAgents := make(map[string]int)
	lostNodeAllocationFound := false
	for _, allocation := range allocations {
		if allocation.RolloutState == deliverycore.AllocationRolloutLost {
			if allocation.AgentID == "node-b" {
				lostNodeAllocationFound = true
			}
			continue
		}
		activeAgents[allocation.AgentID]++
	}
	if !lostNodeAllocationFound {
		t.Fatalf("expected node-b allocation to be retained as lost, got %+v", allocations)
	}
	if activeAgents["node-b"] != 0 {
		t.Fatalf("lost node still has an active replica: %v", activeAgents)
	}
	if activeAgents["node-a"] == 2 {
		t.Fatalf("failover colocated onto the surviving replica when node-c had capacity: %v", activeAgents)
	}
	if activeAgents["node-c"] != 1 {
		t.Fatalf("expected failover onto node-c, got %v", activeAgents)
	}
}

func TestReplicaConcurrentScalingStaysConsistent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b", "node-c"})
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		desired := int32(2 + i%2)
		go func(desired int32) {
			defer wg.Done()
			_, _, err := scaleService(ctx, store, "user-1", service.ID, desired)
			if err != nil && !errors.Is(err, deliverycore.ErrConcurrentUpdate) {
				errs <- err
			}
		}(desired)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent scale: %v", err)
	}

	current, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued := current.Spec.GetDesiredReplicaCount()
	if queued != 2 && queued != 3 {
		t.Fatalf("queued replica count settled at %d, want 2 or 3", queued)
	}
	if current.DesiredReplicaCount != 1 {
		t.Fatalf("live desired replica count = %d, want 1 until deploy", current.DesiredReplicaCount)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	current, err = store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if int32(len(allocations)) > current.DesiredReplicaCount {
		t.Fatalf("placed %d allocations above desired %d", len(allocations), current.DesiredReplicaCount)
	}
	if current.DesiredReplicaCount != 2 && current.DesiredReplicaCount != 3 {
		t.Fatalf("desired replica count settled at %d, want 2 or 3", current.DesiredReplicaCount)
	}
}

func mustQueueAndDeployReplicas(t *testing.T, store *persistence, ctx context.Context, envID, serviceID string, desired int32) {
	t.Helper()
	if _, _, err := scaleService(ctx, store, "user-1", serviceID, desired); err != nil {
		t.Fatalf("scaleService: %v", err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
}

func replicaSpec(cpu, memory int64) *platformv1.ServiceSpec {
	return directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       cpu,
		MemoryMebibytes: memory,
		Ports:           runtimePortsFromInts([]int32{8080}),
	})
}

func seedReplicaFixture(t *testing.T, store *persistence, ctx context.Context, agentIDs []string) string {
	t.Helper()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	for _, agentID := range agentIDs {
		hello := agentHello(agentID)
		hello.AdvertiseAddr = "fd00:30::" + strings.TrimPrefix(agentID, "node-")
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatalf("upsertAgent(%s): %v", agentID, err)
		}
	}
	return productionEnvironmentID(t, store, projects[0].ID)
}

func mustListAllocations(t *testing.T, store *persistence, ctx context.Context, serviceID string) []deliverycore.AllocationRecord {
	t.Helper()
	allocations, err := store.reads.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		t.Fatalf("listAllocationsByServiceID: %v", err)
	}
	return allocations
}

func allocationAgentIDs(allocations []deliverycore.AllocationRecord) map[string]int {
	out := map[string]int{}
	for _, alloc := range allocations {
		out[alloc.AgentID]++
	}
	return out
}
