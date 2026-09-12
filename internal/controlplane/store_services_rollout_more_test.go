//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestDirectImageEnvironmentReleaseUpdatesDesiredImage(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("example.test/web:a", nil), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", directImageServiceSpec("example.test/web:b", nil)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	before, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent before environment release: %v", err)
	}
	if got := before.GetServices()[0].GetSpec().GetImage(); got != "example.test/web:a" {
		t.Fatalf("draft image leaked before environment release: got %q", got)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	after, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent after environment release: %v", err)
	}
	if got := after.GetServices()[0].GetSpec().GetImage(); got != "example.test/web:b" {
		t.Fatalf("environment release kept stale image: got %q", got)
	}
}

func TestDomainBindingChangesBumpDesiredRevisions(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	node1AfterService := mustDesiredRevision(t, store, ctx, "node-1")

	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	node1AfterDeleteService := mustDesiredRevision(t, store, ctx, "node-1")
	if node1AfterDeleteService != node1AfterService+1 {
		t.Fatalf("expected node-1 revision %d after deleteService, got %d", node1AfterService+1, node1AfterDeleteService)
	}

	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if got := state.GetReconciliationCursor(); got != node1AfterDeleteService {
		t.Fatalf("expected desired state revision %d, got %d", node1AfterDeleteService, got)
	}

	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", service.ID, 8080); err == nil {
		t.Fatal("expected createDomainBinding for deleted service to fail")
	}

	service, err = createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-2", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(second): %v", err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "source.platform.example", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	node1BeforeDomains := mustDesiredRevision(t, store, ctx, "node-1")

	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDomains+1 {
		t.Fatalf("expected revision bumped after createDomainBinding, got %d want %d", got, node1BeforeDomains+1)
	}

	otherService, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-3", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(third): %v", err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "target.platform.example", otherService.ID, 8080); err != nil {
		t.Fatal(err)
	}
	beforeUpdate := mustDesiredRevision(t, store, ctx, "node-1")
	if _, _, err := store.routing.UpdateDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", otherService.ID, 8080); err != nil {
		t.Fatalf("updateDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != beforeUpdate+1 {
		t.Fatalf("expected revision %d after updating domain, got %d", beforeUpdate+1, got)
	}

	node1BeforeDeleteDomain := mustDesiredRevision(t, store, ctx, "node-1")
	if _, err := store.routing.DeleteDomainBindingRecord(ctx, testUser("user-1"), "web.example.com"); err != nil {
		t.Fatalf("deleteDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDeleteDomain+1 {
		t.Fatalf("expected revision bumped after deleteDomainBinding, got %d want %d", got, node1BeforeDeleteDomain+1)
	}
}

func TestUnusedAgentAdvertiseAddressDoesNotBumpDesiredRevisions(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	node1Before := mustDesiredRevision(t, store, ctx, "node-1")
	node2Before := mustDesiredRevision(t, store, ctx, "node-2")

	hello := agentHello("node-1")
	hello.AdvertiseAddr = "fd00:30::11"
	changed, err := upsertTestAgent(t, store, ctx, hello)
	if err != nil {
		t.Fatalf("upsertAgent: %v", err)
	}
	if !changed {
		t.Fatal("expected agent topology change to be detected")
	}

	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before {
		t.Fatalf("unused advertise address bumped node-1 revision from %d to %d", node1Before, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before {
		t.Fatalf("unused advertise address bumped node-2 revision from %d to %d", node2Before, got)
	}
}

func TestRecordStatusReportTracksIngressVisibleChanges(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}

	routedService, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(routed): %v", err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", routedService.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	internalService, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "worker", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(internal): %v", err)
	}

	routedAlloc := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, routedService.ID)
	internalAlloc := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, internalService.ID)

	changed, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIpv4:      routedAlloc.AllocationIPv4,
			AllocationIpv6:      routedAlloc.AllocationIPv6,
			HealthyIpv4Ports:    []int32{8080},
			HealthyIpv6Ports:    []int32{8080},
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed changed): %v", err)
	}
	if !changed {
		t.Fatal("expected routed status change to trigger ingress update")
	}
	changed, _, err = testDelivery(store).recordStatusReport(ctx, "node-2", &agentv1.StatusReport{
		AgentId: "node-2",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:   routedAlloc.ID,
			AllocationIpv4: "10.0.0.99",
			Healthy:        false,
		}},
	})
	if !errors.Is(err, deliverycore.ErrAllocationOwnership) {
		t.Fatalf("recordStatusReport(foreign agent): got %v", err)
	}
	if changed {
		t.Fatal("expected foreign agent report to leave ingress unchanged")
	}
	routedAllocAfterForeignReport := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, routedService.ID)
	expectedAllocationIPv6 := routedAlloc.AllocationIPv6
	if routedAllocAfterForeignReport.AllocationIPv6 != expectedAllocationIPv6 || !routedAllocAfterForeignReport.Healthy {
		t.Fatalf("foreign agent changed allocation state: %+v", routedAllocAfterForeignReport)
	}
	changed, _, err = testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIpv4:      routedAlloc.AllocationIPv4,
			AllocationIpv6:      routedAlloc.AllocationIPv6,
			HealthyIpv4Ports:    []int32{8080},
			HealthyIpv6Ports:    []int32{8080},
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed unchanged): %v", err)
	}
	if changed {
		t.Fatal("expected unchanged routed status to skip ingress update")
	}

	changed, _, err = testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        internalAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIpv4:      internalAlloc.AllocationIPv4,
			AllocationIpv6:      internalAlloc.AllocationIPv6,
			HealthyIpv4Ports:    []int32{8080},
			HealthyIpv6Ports:    []int32{8080},
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(internal changed): %v", err)
	}
	if changed {
		t.Fatal("expected non-routed status change to skip ingress update")
	}
}

func TestChooseAgentForServiceUsesDatabaseAggregation(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-b"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	agentID, err := chooseAgentForService(ctx, store, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}))
	if err != nil {
		t.Fatalf("chooseAgentForService: %v", err)
	}
	if agentID != "node-a" {
		t.Fatalf("expected node-a, got %q", agentID)
	}
}

func TestChooseAgentForServiceRejectsOverCapacityAgents(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	hello := agentHello("node-1")
	hello.CpuMillisCapacity = 500
	hello.MemoryMebibytesCapacity = 512
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       400,
		MemoryMebibytes: 256,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	_, err = chooseAgentForService(ctx, store, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       200,
		MemoryMebibytes: 300,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}))
	if !errors.Is(err, deliverycore.ErrNoPlacementAvailable) {
		t.Fatalf("expected errNoPlacementAvailable, got %v", err)
	}
}

func mustPrimaryAllocation(t *testing.T, store *persistence, ctx context.Context, userID, projectID, serviceID string) deliverycore.AllocationRecord {
	t.Helper()
	_, allocations, err := store.reads.ServiceStatus(ctx, testUser(userID), serviceID)
	if err != nil {
		t.Fatalf("ServiceStatus(%s): %v", serviceID, err)
	}
	return primaryAllocation(allocations)
}

func mustDesiredRevision(t *testing.T, store *persistence, ctx context.Context, agentID string) int64 {
	t.Helper()

	revision, err := store.currentDesiredRevisionForAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("currentDesiredRevisionForAgent(%s): %v", agentID, err)
	}
	return revision
}
