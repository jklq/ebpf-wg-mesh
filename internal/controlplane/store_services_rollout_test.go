//go:build integration

package controlplane

import (
	"context"
	"testing"

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
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
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
	deployed, _, err := releaseEnvironmentForTest(ctx, store, "user-1", environmentID)
	if err != nil || len(deployed) != 2 {
		t.Fatalf("releaseEnvironment: %#v: %v", deployed, err)
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

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i, port := range []int32{8081, 8082} {
		i := i
		port := port
		go func() {
			<-start
			_, _, err := store.updateService(ctx, "user-1", service.ID, "", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
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

	current, err := store.serviceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("serviceByID: %v", err)
	}
	if current.SpecRevision != 3 {
		t.Fatalf("expected current spec revision 3, got %d", current.SpecRevision)
	}
	if current.RolloutGeneration != 1 {
		t.Fatalf("expected rollout generation to remain 1 before release, got %d", current.RolloutGeneration)
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
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
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
	updated, changed, err := store.updateService(ctx, "user-1", service.ID, "", canonicalServiceSpec(spec))
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
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
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

	if err := store.deleteService(ctx, "user-1", service.ID); err != nil {
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
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
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

	updated, changed, err := store.updateService(ctx, "user-1", service.ID, "talented-harmony", canonicalServiceSpec(spec))
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

func TestExactRedeployCopiesImmutableSnapshotIntoNewRollout(t *testing.T) {
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

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("currentDeploymentForService: ok=%v err=%v", ok, err)
	}
	redeployed, _, err := store.applyDeploymentAction(ctx, "user-1", service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "exact-redeploy", "")
	if err != nil {
		t.Fatalf("applyDeploymentAction(EXACT_REDEPLOY): %v", err)
	}
	if redeployed.SpecRevision != 2 {
		t.Fatalf("expected copied snapshot at spec revision 2, got %d", redeployed.SpecRevision)
	}
	if redeployed.RolloutGeneration != 2 {
		t.Fatalf("expected rollout generation 2, got %d", redeployed.RolloutGeneration)
	}
	revisions, err := store.countServiceRevisionsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRevisionsForTest: %v", err)
	}
	if revisions != 2 {
		t.Fatalf("expected 2 stored spec revisions after exact redeploy, got %d", revisions)
	}
	rollouts, err := store.countServiceRolloutsForTest(ctx, service.ID)
	if err != nil {
		t.Fatalf("countServiceRolloutsForTest: %v", err)
	}
	if rollouts != 2 {
		t.Fatalf("expected 2 stored rollouts after redeploy, got %d", rollouts)
	}
	allocation := mustPrimaryAllocation(t, store, ctx, "user-1", projects[0].ID, service.ID)
	if allocation.DesiredSpecRevision != 2 || allocation.DesiredRolloutGeneration != 2 {
		t.Fatalf("unexpected desired allocation state: %+v", allocation)
	}
}
