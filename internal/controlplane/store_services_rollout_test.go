//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestConcurrentCreateServicePlacementIsAtomic(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}

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
			rec, err := store.createScheduledService(ctx, "user-1", projects[0].ID, name, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
				CpuMillis:       100,
				MemoryMebibytes: 64,
				ContainerPort:   8080,
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
	if first.rec.AllocatedAgentID == second.rec.AllocatedAgentID {
		t.Fatalf("expected placement to spread across agents, both services landed on %q", first.rec.AllocatedAgentID)
	}
}

func TestConcurrentUpdateServiceAdvancesUniqueRevisions(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
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
			_, _, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
				CpuMillis:       100,
				MemoryMebibytes: 64 + int64(i),
				ContainerPort:   port,
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
	if current.RolloutGeneration != 3 {
		t.Fatalf("expected rollout generation 3, got %d", current.RolloutGeneration)
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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
		ContainerPort:   8080,
	})
	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	updated, changed, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, canonicalServiceSpec(spec))
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

func TestRedeployServiceAdvancesRolloutOnly(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
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
	_, allocation, err := store.serviceStatus(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if allocation.DesiredSpecRevision != 1 || allocation.DesiredRolloutGeneration != 2 {
		t.Fatalf("unexpected desired allocation state: %+v", allocation)
	}
}

func TestDesiredRevisionsIgnoreDomainBindingChanges(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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
	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
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

	// Domain bindings are control-plane ingress state, not part of agent desired state.
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", service.ID); err == nil {
		t.Fatal("expected createDomainBinding for deleted service to fail")
	}

	service, err = store.createService(ctx, "user-1", projects[0].ID, "web-2", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(second): %v", err)
	}
	node1BeforeDomains := mustDesiredRevision(t, store, ctx, "node-1")

	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", service.ID); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDomains {
		t.Fatalf("expected revision unchanged after createDomainBinding, got %d want %d", got, node1BeforeDomains)
	}

	otherService, err := store.createService(ctx, "user-1", projects[0].ID, "web-3", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
	}), "node-1")
	if err != nil {
		t.Fatalf("createService(third): %v", err)
	}
	if _, _, err := store.updateDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", otherService.ID); err != nil {
		t.Fatalf("updateDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDomains+1 {
		t.Fatalf("expected revision %d after creating second service only, got %d", node1BeforeDomains+1, got)
	}

	node1BeforeDeleteDomain := mustDesiredRevision(t, store, ctx, "node-1")
	if _, err := store.deleteDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com"); err != nil {
		t.Fatalf("deleteDomainBinding: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1BeforeDeleteDomain {
		t.Fatalf("expected revision unchanged after deleteDomainBinding, got %d want %d", got, node1BeforeDeleteDomain)
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	routedService, err := store.createService(ctx, "user-1", projects[0].ID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(routed): %v", err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.com", routedService.ID); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	internalService, err := store.createService(ctx, "user-1", projects[0].ID, "worker", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService(internal): %v", err)
	}

	_, routedAlloc, err := store.serviceStatus(ctx, "user-1", projects[0].ID, routedService.ID)
	if err != nil {
		t.Fatalf("serviceStatus(routed): %v", err)
	}
	_, internalAlloc, err := store.serviceStatus(ctx, "user-1", projects[0].ID, internalService.ID)
	if err != nil {
		t.Fatalf("serviceStatus(internal): %v", err)
	}

	changed, err := store.recordStatusReport(ctx, &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			EndpointAddr:        "10.0.0.10:8080",
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed changed): %v", err)
	}
	if !changed {
		t.Fatal("expected routed status change to trigger ingress update")
	}

	changed, err = store.recordStatusReport(ctx, &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        routedAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			EndpointAddr:        "10.0.0.10:8080",
			Healthy:             true,
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport(routed unchanged): %v", err)
	}
	if changed {
		t.Fatal("expected unchanged routed status to skip ingress update")
	}

	changed, err = store.recordStatusReport(ctx, &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:        internalAlloc.ID,
			AppliedSpecRevision: 1,
			Phase:               "Running",
			EndpointAddr:        "10.0.0.11:8080",
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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
	if _, err := store.createService(ctx, "user-1", projects[0].ID, "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
	}), "node-b"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	agentID, err := store.chooseAgentForService(ctx, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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
	if _, err := store.createService(ctx, "user-1", projects[0].ID, "existing", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       400,
		MemoryMebibytes: 256,
		ContainerPort:   8080,
	}), "node-1"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	_, err = store.chooseAgentForService(ctx, projects[0].ID, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       200,
		MemoryMebibytes: 300,
		ContainerPort:   8080,
	}))
	if !errors.Is(err, errNoPlacementAvailable) {
		t.Fatalf("expected errNoPlacementAvailable, got %v", err)
	}
}

func mustDesiredRevision(t *testing.T, store *Store, ctx context.Context, agentID string) int64 {
	t.Helper()

	revision, err := store.currentDesiredRevisionForAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("currentDesiredRevisionForAgent(%s): %v", agentID, err)
	}
	return revision
}
