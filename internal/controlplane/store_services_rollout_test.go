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

func TestConcurrentCreateServicePlacementIsAtomic(t *testing.T) {
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
	environmentID := productionEnvironmentID(t, store, projects[0].ID)

	for _, id := range []string{"node-a", "node-b"} {
		hello := agentHello(id)
		hello.CpuMillisCapacity = 100
		hello.MemoryMebibytesCapacity = 128
		if _, err := store.upsertAgent(ctx, hello); err != nil {
			t.Fatalf("upsertAgent(%s): %v", id, err)
		}
	}

	start := make(chan struct{})
	type result struct {
		rec serviceRecord
		err error
	}
	results := make(chan result, 2)
	for _, name := range []string{"web-a", "web-b"} {
		name := name
		go func() {
			<-start
			rec, err := store.createScheduledService(ctx, "user-1", environmentID, name, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
				CpuMillis:       100,
				MemoryMebibytes: 64,
				Ports:           runtimePortsFromInts([]int32{8080}),
			}))
			results <- result{rec: rec, err: err}
		}()
	}

	close(start)
	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatalf("first createScheduledService: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second createScheduledService: %v", second.err)
	}
	deployed, _, err := store.deployEnvironment(ctx, "user-1", environmentID)
	if err != nil || len(deployed) != 2 {
		t.Fatalf("deployEnvironment: %#v: %v", deployed, err)
	}
	if deployed[0].AllocatedAgentID == deployed[1].AllocatedAgentID {
		t.Fatalf("expected placement to spread across agents, both services landed on %q", deployed[0].AllocatedAgentID)
	}
}

func TestConcurrentUpdateServiceAdvancesUniqueRevisions(t *testing.T) {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i, port := range []int32{8081, 8082} {
		i := i
		port := port
		go func() {
			<-start
			_, _, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
				CpuMillis:       100,
				MemoryMebibytes: 64 + int64(i),
				Ports:           runtimePortsFromInts([]int32{port}),
			}))
			errs <- err
		}()
	}

	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("updateService: %v", err)
		}
	}

	current, err := store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID: %v", err)
	}
	if current.SpecRevision != 3 {
		t.Fatalf("expected current spec revision 3, got %d", current.SpecRevision)
	}
	if current.RolloutGeneration != 1 {
		t.Fatalf("expected rollout generation to remain 1 before deploy, got %d", current.RolloutGeneration)
	}
	if !current.PendingChanges {
		t.Fatal("expected updated service to report pending changes")
	}
	var revisions int
	if revisions, err = store.countServiceRevisionsForTest(ctx, service.ID); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if revisions != 3 {
		t.Fatalf("expected 3 stored revisions, got %d", revisions)
	}
}

func TestUpdateServiceNoopDoesNotAdvanceSpecOrRollout(t *testing.T) {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	})
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	updated, changed, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "", canonicalServiceSpec(spec))
	if err != nil {
		t.Fatalf("updateService noop: %v", err)
	}
	if changed {
		t.Fatal("expected noop update to report changed=false")
	}
	if updated.SpecRevision != 1 || updated.RolloutGeneration != 1 {
		t.Fatalf("expected no-op update to preserve revisions, got spec=%d rollout=%d", updated.SpecRevision, updated.RolloutGeneration)
	}
	revisions, err := store.countServiceRevisionsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRevisionsForTest: %v", err)
	}
	if revisions != 1 {
		t.Fatalf("expected 1 stored spec revision after noop, got %d", revisions)
	}
	rollouts, err := store.countServiceRolloutsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRolloutsForTest: %v", err)
	}
	if rollouts != 1 {
		t.Fatalf("expected 1 stored rollout after noop, got %d", rollouts)
	}
}

func TestServiceCreateAndDeleteUpdateWorkloadAndNetworkState(t *testing.T) {
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
	for _, agentID := range []string{"node-1", "node-2"} {
		if _, err := store.upsertAgent(ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsertAgent(%s): %v", agentID, err)
		}
	}

	node1Before := mustDesiredRevision(t, store, ctx, "node-1")
	node2Before := mustDesiredRevision(t, store, ctx, "node-2")
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := store.createService(ctx, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("expected node-1 revision %d after create, got %d", node1Before+1, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before+1 {
		t.Fatalf("service create did not refresh node-2 identity catalog: got %d want %d", got, node2Before+1)
	}
	node2State, err := store.desiredStateForAgent(ctx, "node-2")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-2): %v", err)
	}
	if len(node2State.GetServices()) != 0 {
		t.Fatalf("unaffected node-2 received service-local desired state: %#v", node2State.GetServices())
	}
	identities := node2State.GetNodeConfig().GetWorkloadIdentities()
	if len(identities) != 1 || identities[0].GetHostAgentId() != "node-1" {
		t.Fatalf("node-2 did not receive the new cross-node identity: %#v", identities)
	}

	if err := store.deleteService(ctx, "user-1", environmentID, service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+2 {
		t.Fatalf("expected node-1 revision %d after delete, got %d", node1Before+2, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before+2 {
		t.Fatalf("service delete did not refresh node-2 network state: got %d want %d", got, node2Before+2)
	}
}

func TestUpdateServiceNameDoesNotAdvanceSpecOrRollout(t *testing.T) {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	})
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	beforeRevision := mustDesiredRevision(t, store, ctx, "node-1")

	updated, changed, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "talented-harmony", canonicalServiceSpec(spec))
	if err != nil {
		t.Fatalf("updateService rename: %v", err)
	}
	if changed {
		t.Fatal("expected rename-only update to report changed=false")
	}
	if updated.Name != "talented-harmony" {
		t.Fatalf("expected renamed service, got %q", updated.Name)
	}
	if updated.SpecRevision != 1 || updated.RolloutGeneration != 1 {
		t.Fatalf("expected rename to preserve revisions, got spec=%d rollout=%d", updated.SpecRevision, updated.RolloutGeneration)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != beforeRevision+1 {
		t.Fatalf("expected rename to refresh internal hosts, desired revision=%d want %d", got, beforeRevision+1)
	}
	desired, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := desired.GetServices()[0].GetInternalHostname(); got != "talented-harmony.mesh.internal" {
		t.Fatalf("renamed service kept internal hostname %q", got)
	}
}

func TestRedeployServiceAdvancesRolloutOnly(t *testing.T) {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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

	redeployed, err := store.redeployService(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	if redeployed.SpecRevision != 1 {
		t.Fatalf("expected spec revision to remain 1, got %d", redeployed.SpecRevision)
	}
	if redeployed.RolloutGeneration != 2 {
		t.Fatalf("expected rollout generation 2, got %d", redeployed.RolloutGeneration)
	}
	revisions, err := store.countServiceRevisionsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRevisionsForTest: %v", err)
	}
	if revisions != 1 {
		t.Fatalf("expected 1 stored spec revision after redeploy, got %d", revisions)
	}
	rollouts, err := store.countServiceRolloutsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRolloutsForTest: %v", err)
	}
	if rollouts != 2 {
		t.Fatalf("expected 2 stored rollouts after redeploy, got %d", rollouts)
	}
	allocation := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, service.ID)
	if allocation.DesiredSpecRevision != 1 || allocation.DesiredRolloutGeneration != 2 {
		t.Fatalf("unexpected desired allocation state: %+v", allocation)
	}
}

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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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

	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	node1Before := mustDesiredRevision(t, store, ctx, "node-1")
	node2Before := mustDesiredRevision(t, store, ctx, "node-2")

	hello := agentHello("node-1")
	hello.Name = "node-1-renamed"
	changed, err := store.upsertAgent(ctx, hello)
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
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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
	if _, err := store.upsertAgent(ctx, agentHello("node-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-b")); err != nil {
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
	if _, err := store.upsertAgent(ctx, hello); err != nil {
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
