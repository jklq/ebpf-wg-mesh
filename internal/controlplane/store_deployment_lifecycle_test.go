//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"sync"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestDeploymentLifecyclePersistsHappyPathAndHistory(t *testing.T) {
	t.Parallel()

	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Add lifecycle", "Ada"); err != nil {
		t.Fatal(err)
	}

	current := currentDeploymentForTest(t, store, ctx, service.ID)
	if current.State != deliverycore.DeploymentStateStaged {
		t.Fatalf("create state = %q, want staged", current.State)
	}

	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	queued := currentDeploymentForTest(t, store, ctx, service.ID)
	if queued.State != deliverycore.DeploymentStateQueuedBuild || queued.BuildID != build.ID {
		t.Fatalf("queued deployment = %+v", queued)
	}

	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	building := currentDeploymentForTest(t, store, ctx, service.ID)
	if building.State != deliverycore.DeploymentStateBuilding {
		t.Fatalf("building deployment = %+v", building)
	}

	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:1111111111111111111111111111111111111111111111111111111111111111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}
	scheduled := currentDeploymentForTest(t, store, ctx, service.ID)
	if scheduled.State != deliverycore.DeploymentStateScheduling || scheduled.ImageDigest == "" {
		t.Fatalf("scheduled deployment = %+v", scheduled)
	}

	alloc, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil || alloc.ID == "" {
		t.Fatalf("allocation: %+v err=%v", alloc, err)
	}
	if _, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			AllocationIpv4:           alloc.AllocationIPv4,
			AllocationIpv6:           alloc.AllocationIPv6,
			DesiredSpecRevision:      alloc.DesiredSpecRevision,
			AppliedSpecRevision:      alloc.DesiredSpecRevision,
			DesiredRolloutGeneration: scheduled.RolloutGeneration,
			AppliedRolloutGeneration: scheduled.RolloutGeneration,
			Phase:                    "Healthy",
			Healthy:                  true,
			Message:                  "HTTP readiness check passed",
		}},
	}); err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
	active := currentDeploymentForTest(t, store, ctx, service.ID)
	if active.State != deliverycore.DeploymentStateActive {
		t.Fatalf("active deployment = %+v", active)
	}

	history, err := store.reads.ListServiceDeployments(ctx, testUser(userID), service.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments: %v", err)
	}
	if len(history) < 2 {
		t.Fatalf("expected create + build deployments, got %d", len(history))
	}
	if history[0].State != deliverycore.DeploymentStateActive || history[0].Build == nil || history[0].Build.CommitMessage != "Add lifecycle" {
		t.Fatalf("latest history = %+v", history[0])
	}
	if history[0].ReasonCode != "DEPLOYMENT_ACTIVE" || history[0].CauseKind != deliverycore.DeploymentCauseSystem {
		t.Fatalf("latest transition metadata = %+v", history[0])
	}
}

func TestAgentCannotResurrectTerminalDeployment(t *testing.T) {
	t.Parallel()

	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "docker build failed"); err != nil {
		t.Fatal(err)
	}
	failed := currentDeploymentForTest(t, store, ctx, service.ID)
	if failed.State != deliverycore.DeploymentStateFailed {
		t.Fatalf("failed deployment = %+v", failed)
	}

	alloc, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			DesiredRolloutGeneration: failed.RolloutGeneration,
			AppliedRolloutGeneration: failed.RolloutGeneration,
			AllocationIpv4:           alloc.AllocationIPv4,
			AllocationIpv6:           alloc.AllocationIPv6,
			Phase:                    "Healthy",
			Healthy:                  true,
			Message:                  "should not resurrect",
		}},
	}); err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
	stillFailed := currentDeploymentForTest(t, store, ctx, service.ID)
	if stillFailed.State != deliverycore.DeploymentStateFailed {
		t.Fatalf("agent resurrected terminal deployment: %+v", stillFailed)
	}
}

func TestDeploymentTransitionRetriesAreIdempotent(t *testing.T) {
	t.Parallel()

	store, ctx, _, _, service := setupDirectImageServiceForDeployment(t)
	_ = currentDeploymentForTest(t, store, ctx, service.ID)
	activate := func() error { return reportActiveForTest(ctx, store, service.ID) }
	if err := activate(); err != nil {
		t.Fatalf("first activate: %v", err)
	}
	if err := activate(); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	active := currentDeploymentForTest(t, store, ctx, service.ID)
	if active.State != deliverycore.DeploymentStateActive {
		t.Fatalf("after retries: %+v", active)
	}
}

func TestDeploymentRacesWebhookUserBuilderAndAgent(t *testing.T) {
	t.Parallel()

	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Race commit", "Ada"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-1")
		errs <- err
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := releaseEnvironmentServiceForTest(ctx, store, userID, service.EnvironmentID, service.ID)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	var success int
	for err := range errs {
		if err == nil {
			success++
		}
	}
	if success == 0 {
		t.Fatal("expected at least one of webhook enqueue or environment release to succeed")
	}

	var currentCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM deployments WHERE service_id = $1 AND is_current = TRUE`, service.ID).Scan(&currentCount); err != nil {
		t.Fatal(err)
	}
	if currentCount != 1 {
		t.Fatalf("expected exactly one current deployment after webhook/user race, got %d", currentCount)
	}

	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueue for builder/agent race: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-race", build.ID)
	alloc, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	current := currentDeploymentForTest(t, store, ctx, service.ID)

	var raceWG sync.WaitGroup
	raceErrs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			raceErrs <- completeBuildForTest(ctx, store, "builder-race", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:cececececececececececececececececececececececececececececececece", "")
		}()
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			_, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
				AgentId: "node-1",
				Services: []*agentv1.ServiceCondition{{
					AllocationId:             alloc.ID,
					ServiceId:                service.ID,
					AllocationIpv4:           alloc.AllocationIPv4,
					AllocationIpv6:           alloc.AllocationIPv6,
					DesiredRolloutGeneration: current.RolloutGeneration,
					AppliedRolloutGeneration: current.RolloutGeneration,
					Phase:                    "Healthy",
					Healthy:                  true,
					Message:                  "agent retry",
				}},
			})
			raceErrs <- err
		}()
	}
	raceWG.Wait()
	close(raceErrs)
	for err := range raceErrs {
		if err != nil && !errors.Is(err, deliverycore.ErrBuildNotOwned) && !errors.Is(err, deliverycore.ErrStaleObservation) {
			t.Fatalf("builder/agent race: %v", err)
		}
	}

	final := currentDeploymentForTest(t, store, ctx, service.ID)
	if deploymentStateTerminal(final.State) && final.State != deliverycore.DeploymentStateFailed && final.State != deliverycore.DeploymentStateSuperseded {
		// terminal is allowed; agent must not have moved a failed/superseded row back to active
	}
	if final.State == deliverycore.DeploymentStateFailed || final.State == deliverycore.DeploymentStateSuperseded || final.State == deliverycore.DeploymentStateCancelled || final.State == deliverycore.DeploymentStateCrashed || final.State == deliverycore.DeploymentStateRemoved {
		if err := reportActiveForTest(ctx, store, service.ID); err != nil {
			t.Fatalf("agent resurrect after race: %v", err)
		}
		after := currentDeploymentForTest(t, store, ctx, service.ID)
		if after.State != final.State {
			t.Fatalf("agent changed terminal state from %q to %+v", final.State, after)
		}
	}
}

func setupSourceServiceForDeployment(t *testing.T) (*persistence, context.Context, string, string, deliverycore.ServiceRecord) {
	t.Helper()
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
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}

func setupDirectImageServiceForDeployment(t *testing.T) (*persistence, context.Context, string, string, deliverycore.ServiceRecord) {
	t.Helper()
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
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}

func currentDeploymentForTest(t *testing.T, store *persistence, ctx context.Context, serviceID string) deliverycore.DeploymentRecord {
	t.Helper()
	snapshot, err := store.reads.ServiceSnapshot(ctx, serviceID)
	if err != nil {
		t.Fatalf("ServiceSnapshot(%q): %v", serviceID, err)
	}
	if snapshot.LatestDeployment == nil {
		t.Fatalf("expected current deployment for service %q", serviceID)
	}
	return *snapshot.LatestDeployment
}
