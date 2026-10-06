//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/registry"
)

func TestConcurrentCreateServicePlacementIsAtomic(t *testing.T) {
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
		rec deliverycore.ServiceRecord
		err error
	}
	results := make(chan result, 2)
	for _, name := range []string{"web-a", "web-b"} {
		name := name
		go func() {
			<-start
			rec, err := createScheduledService(ctx, store, "user-1", environmentID, name, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
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
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i, port := range []int32{8081, 8082} {
		i := i
		port := port
		go func() {
			<-start
			// Both writers read the same draft revision, so one loses the
			// optimistic guard. A real client re-reads and replays its edit; the
			// point here is that the retry lands on its own revision instead of
			// overwriting the winner's.
			var err error
			for attempt := 0; attempt < 5; attempt++ {
				_, _, err = updateService(ctx, store, "user-1", service.ID, "", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
					CpuMillis:       100,
					MemoryMebibytes: 64 + int64(i),
					Ports:           runtimePortsFromInts([]int32{port}),
				}))
				if !errors.Is(err, deliverycore.ErrConcurrentUpdate) {
					break
				}
			}

			errs <- err
		}()
	}

	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("updateService: %v", err)
		}
	}

	current, err := store.reads.ServiceByID(ctx, testUser("user-1"), service.ID)
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
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	updated, changed, err := updateService(ctx, store, "user-1", service.ID, "", deliverycore.CanonicalServiceSpec(spec))
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
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	service, err := createService(ctx, store, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("expected node-1 revision %d after create, got %d", node1Before+1, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before {
		t.Fatalf("service create refreshed idle node-2: got %d want %d", got, node2Before)
	}
	node2State, err := desiredStateForAgent(ctx, store, "node-2")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-2): %v", err)
	}
	if len(node2State.GetServices()) != 0 {
		t.Fatalf("unaffected node-2 received service-local desired state: %#v", node2State.GetServices())
	}
	if identities := node2State.GetNodeConfig().GetWorkloadIdentities(); len(identities) != 0 {
		t.Fatalf("idle node-2 received an unrelated environment identity: %#v", identities)
	}

	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+2 {
		t.Fatalf("expected node-1 revision %d after delete, got %d", node1Before+2, got)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before {
		t.Fatalf("service delete refreshed idle node-2: got %d want %d", got, node2Before)
	}
}

func TestUpdateServiceNameDoesNotAdvanceSpecOrRollout(t *testing.T) {
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
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	beforeRevision := mustDesiredRevision(t, store, ctx, "node-1")

	updated, changed, err := updateService(ctx, store, "user-1", service.ID, "talented-harmony", deliverycore.CanonicalServiceSpec(spec))
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
	desired, err := desiredStateForAgent(ctx, store, "node-1")
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
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	current := currentDeploymentForTest(t, store, ctx, service.ID)
	redeployed, _, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "exact-redeploy", "")
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

// testPinnedImage resolves a direct-image input the way the delivery test
// harness does, returning the digest-pinned runtime identity.
func testPinnedImage(t *testing.T, input string) string {
	t.Helper()
	resolved, err := registry.StaticResolverForTest().Resolve(context.Background(), input)
	if err != nil {
		t.Fatalf("resolve %s: %v", input, err)
	}
	return resolved.Ref
}

func TestDirectImageEnvironmentReleaseUpdatesDesiredImage(t *testing.T) {
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
	// The mutable tag is user input only: what is scheduled is the digest it
	// resolved to at deploy time.
	if got := before.GetServices()[0].GetSpec().GetImage(); got != testPinnedImage(t, "example.test/web:a") {
		t.Fatalf("draft image leaked before environment release: got %q want %q", got, testPinnedImage(t, "example.test/web:a"))
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	after, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent after environment release: %v", err)
	}
	if got := after.GetServices()[0].GetSpec().GetImage(); got != testPinnedImage(t, "example.test/web:b") {
		t.Fatalf("environment release kept stale image: got %q want %q", got, testPinnedImage(t, "example.test/web:b"))
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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

func TestRolloutAdvancementRollbackAndConcurrency(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)[0]
	markRolloutAllocationReady(t, store, target)
	now := time.Now().UTC()
	revision := mustDesiredRevision(t, store, ctx, target.AgentID)
	aborted := errors.New("abort after rollout writes")
	deps := deliveryDependencies(store, nil, nil, nil, nil)
	deps.ProductTransaction = func(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
		return store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return aborted
		})
	}
	engine := deliverycore.New(deps)
	engine.SetClocks(func() time.Time { return now }, nil)
	err := engine.ReconcileRollouts(ctx)
	if !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	if got := allocationByID(t, store, service.ID, old.ID); got.RolloutState != deliverycore.AllocationRolloutServing {
		t.Fatalf("withdrawal escaped rollback: %+v", got)
	}
	if got := allocationByID(t, store, service.ID, target.ID); got.RolloutState != deliverycore.AllocationRolloutStarting {
		t.Fatalf("promotion escaped rollback: %+v", got)
	}
	if got := mustDesiredRevision(t, store, ctx, target.AgentID); got != revision {
		t.Fatalf("revision escaped rollback: %d -> %d", revision, got)
	}

	waiting := errors.New("ingress has not converged")
	probe := &blockedRolloutIngress{err: waiting}
	engine = deliverycore.New(deliveryDependencies(store, nil, probe, nil, nil))
	engine.SetClocks(func() time.Time { return now }, nil)
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; outcomes <- engine.ReconcileRollouts(ctx) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-outcomes; !errors.Is(err, waiting) {
			t.Fatalf("expected durable withdrawal awaiting ingress: %v", err)
		}
	}
	if got := mustDesiredRevision(t, store, ctx, target.AgentID); got != revision+1 {
		t.Fatalf("revision = %d, want %d", got, revision+1)
	}
	if got := allocationByID(t, store, service.ID, old.ID); got.RolloutState != deliverycore.AllocationRolloutWithdrawing || got.DrainDeadline.Valid {
		t.Fatalf("drained without ingress confirmation: %+v", got)
	}
}

type blockedRolloutIngress struct{ err error }

func (p *blockedRolloutIngress) RequestSync() {}

func (p *blockedRolloutIngress) Sync(context.Context) error { return p.err }

func (p *blockedRolloutIngress) Converged(context.Context) (bool, error) {
	return p.err == nil, nil
}
