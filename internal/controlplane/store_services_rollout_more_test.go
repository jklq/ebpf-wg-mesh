//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestDirectImageRedeployUpdatesDesiredImage(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("example.test/web:a", nil), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "", directImageServiceSpec("example.test/web:b", nil)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	before, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent before redeploy: %v", err)
	}
	if got := before.GetServices()[0].GetSpec().GetImage(); got != "example.test/web:a" {
		t.Fatalf("draft image leaked before redeploy: got %q", got)
	}
	if _, err := store.redeployService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	after, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent after redeploy: %v", err)
	}
	if got := after.GetServices()[0].GetSpec().GetImage(); got != "example.test/web:b" {
		t.Fatalf("redeploy kept stale image: got %q", got)
	}
}

func TestDomainBindingChangesBumpDesiredRevisions(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	node1AfterService := mustDesiredRevision(t, store, ctx, "node-1")

	if err := store.deleteService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	node1AfterDeleteService := mustDesiredRevision(t, store, ctx, "node-1")
	if node1AfterDeleteService != node1AfterService+1 {
		t.Fatalf("expected node-1 revision %d after deleteService, got %d", node1AfterService+1, node1AfterDeleteService)
	}

	state, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if got := state.GetRevision(); got != node1AfterDeleteService {
		t.Fatalf("expected desired state revision %d, got %d", node1AfterDeleteService, got)
	}

	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", service.ID, 8080); err == nil {
		t.Fatal("expected createDomainBinding for deleted service to fail")
	}

	service, err = store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-2", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(second): %v", err)
	}
	node1BeforeDomains := mustDesiredRevision(t, store, ctx, "node-1")

	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDomains+1 {
		t.Fatalf("expected revision bumped after createDomainBinding, got %d want %d", got, node1BeforeDomains+1)
	}

	otherService, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-3", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(third): %v", err)
	}
	if _, _, err := store.updateDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", otherService.ID, 8080); err != nil {
		t.Fatalf("updateDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDomains+3 {
		t.Fatalf("expected revision %d after creating second service and updating domain, got %d", node1BeforeDomains+3, got)
	}

	node1BeforeDeleteDomain := mustDesiredRevision(t, store, ctx, "node-1")
	if _, err := store.deleteDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com"); err != nil {
		t.Fatalf("deleteDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDeleteDomain+1 {
		t.Fatalf("expected revision bumped after deleteDomainBinding, got %d want %d", got, node1BeforeDeleteDomain+1)
	}
}

func TestAgentTopologyChangesBumpAllDesiredRevisions(t *testing.T) {
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

	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("expected node-1 revision %d, got %d", node1Before+1, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before+1 {
		t.Fatalf("expected node-2 revision %d, got %d", node2Before+1, got)
	}
}

func TestRecordStatusReportTracksIngressVisibleChanges(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	routedService, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(routed): %v", err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", routedService.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	internalService, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "worker", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(internal): %v", err)
	}

	routedAlloc := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, routedService.ID)
	internalAlloc := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, internalService.ID)

	changed, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIp:        "10.0.0.10",
			HealthyPorts:        []int32{8080},
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed changed): %v", err)
	}
	if !changed {
		t.Fatal("expected routed status change to trigger ingress update")
	}
	changed, _, err = store.recordStatusReport(ctx, "node-2", &agentv1.StatusReport{
		AgentId: "node-2",
		Services: []*agentv1.ServiceCondition{{
			AllocationId: routedAlloc.ID,
			AllocationIp: "10.0.0.99",
			Healthy:      false,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(foreign agent): %v", err)
	}
	if changed {
		t.Fatal("expected foreign agent report to leave ingress unchanged")
	}
	routedAllocAfterForeignReport := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, routedService.ID)
	agent, err := store.agentByID(ctx, "node-1")
	if err != nil {
		t.Fatalf("agentByID: %v", err)
	}
	expectedAllocationIP, err := privateIPv6(agent.WorkloadIPv6Subnet, routedService.EnvironmentID, routedAlloc.ID)
	if err != nil {
		t.Fatalf("privateIPv6: %v", err)
	}
	if routedAllocAfterForeignReport.AllocationIP != expectedAllocationIP || !routedAllocAfterForeignReport.Healthy {
		t.Fatalf("foreign agent changed allocation state: %+v", routedAllocAfterForeignReport)
	}
	sentinelUpdatedAt := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, `UPDATE allocations SET updated_at = $1 WHERE id = $2`, sentinelUpdatedAt, routedAlloc.ID); err != nil {
		t.Fatalf("set sentinel allocation updated_at: %v", err)
	}

	changed, _, err = store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIp:        "10.0.0.10",
			HealthyPorts:        []int32{8080},
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed unchanged): %v", err)
	}
	if changed {
		t.Fatal("expected unchanged routed status to skip ingress update")
	}
	routedAllocAfterUnchangedReport := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, routedService.ID)
	if !routedAllocAfterUnchangedReport.UpdatedAt.Equal(sentinelUpdatedAt) {
		t.Fatalf("unchanged status rewrote allocation: updated_at = %v, want %v", routedAllocAfterUnchangedReport.UpdatedAt, sentinelUpdatedAt)
	}

	changed, _, err = store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        internalAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			AllocationIp:        "10.0.0.11",
			HealthyPorts:        []int32{8080},
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-b"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	agentID, err := store.chooseAgentForService(ctx, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	hello := agentHello("node-1")
	hello.CpuMillisCapacity = 500
	hello.MemoryMebibytesCapacity = 512
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       400,
		MemoryMebibytes: 256,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	_, err = store.chooseAgentForService(ctx, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       200,
		MemoryMebibytes: 300,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}))
	if !errors.Is(err, errNoPlacementAvailable) {
		t.Fatalf("expected errNoPlacementAvailable, got %v", err)
	}
}

func mustPrimaryAllocation(t *testing.T, store *Store, ctx context.Context, userID, projectID, serviceID string) allocationRecord {
	t.Helper()
	_, allocations, err := store.serviceStatus(ctx, userID, projectID, serviceID)
	if err != nil {
		t.Fatalf("serviceStatus(%s): %v", serviceID, err)
	}
	return primaryAllocation(allocations)
}

func mustDesiredRevision(t *testing.T, store *Store, ctx context.Context, agentID string) int64 {
	t.Helper()

	revision, err := store.currentDesiredRevisionForAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("currentDesiredRevisionForAgent(%s): %v", agentID, err)
	}
	return revision
}
