//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestDeploymentLifecyclePersistsHappyPathAndHistory(t *testing.T) {
	t.Parallel()

	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Add lifecycle", "Ada"); err != nil {
		t.Fatal(err)
	}

	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("current deployment after create: ok=%v err=%v", ok, err)
	}
	if current.State != deploymentStateStaged {
		t.Fatalf("create state = %q, want staged", current.State)
	}

	build, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}
	queued, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("current after enqueue: ok=%v err=%v", ok, err)
	}
	if queued.State != deploymentStateQueuedBuild || queued.BuildID != build.ID {
		t.Fatalf("queued deployment = %+v", queued)
	}

	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	building, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || building.State != deploymentStateBuilding {
		t.Fatalf("building deployment = %+v ok=%v err=%v", building, ok, err)
	}

	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}
	scheduled, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || scheduled.State != deploymentStateScheduling || scheduled.ImageDigest == "" {
		t.Fatalf("scheduled deployment = %+v ok=%v err=%v", scheduled, ok, err)
	}

	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil || alloc.ID == "" {
		t.Fatalf("allocation: %+v err=%v", alloc, err)
	}
	if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			DesiredRolloutGeneration: scheduled.RolloutGeneration,
			AppliedRolloutGeneration: scheduled.RolloutGeneration,
			Phase:                    "Healthy",
			Healthy:                  true,
			Message:                  "HTTP readiness check passed",
		}},
	}); err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
	active, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || active.State != deploymentStateActive {
		t.Fatalf("active deployment = %+v ok=%v err=%v", active, ok, err)
	}

	history, err := store.listServiceDeployments(ctx, userID, projectID, service.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments: %v", err)
	}
	if len(history) < 2 {
		t.Fatalf("expected create + build deployments, got %d", len(history))
	}
	if history[0].State != deploymentStateActive || history[0].Build == nil || history[0].Build.CommitMessage != "Add lifecycle" {
		t.Fatalf("latest history = %+v", history[0])
	}
	if history[0].ReasonCode != reasonDeploymentActive || history[0].CauseKind != deploymentCauseSystem {
		t.Fatalf("latest transition metadata = %+v", history[0])
	}
}

func TestAgentCannotResurrectTerminalDeployment(t *testing.T) {
	t.Parallel()

	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatal(err)
	}
	build, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "docker build failed"); err != nil {
		t.Fatal(err)
	}
	failed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || failed.State != deploymentStateFailed {
		t.Fatalf("failed deployment = %+v ok=%v err=%v", failed, ok, err)
	}

	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			DesiredRolloutGeneration: failed.RolloutGeneration,
			AppliedRolloutGeneration: failed.RolloutGeneration,
			Phase:                    "Healthy",
			Healthy:                  true,
			Message:                  "should not resurrect",
		}},
	}); err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
	stillFailed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || stillFailed.State != deploymentStateFailed {
		t.Fatalf("agent resurrected terminal deployment: %+v", stillFailed)
	}
}

func TestDeploymentTransitionRetriesAreIdempotent(t *testing.T) {
	t.Parallel()

	store, ctx, _, _, service := setupDirectImageServiceForDeployment(t)
	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("current: ok=%v err=%v", ok, err)
	}
	activate := func() error {
		return store.withTx(ctx, func(tx *sql.Tx) error {
			_, err := store.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
				ToState:    deploymentStateActive,
				Actor:      deploymentActor{Kind: deploymentCauseSystem},
				ReasonCode: reasonDeploymentActive,
				Detail:     "activate",
			})
			return err
		})
	}
	if err := activate(); err != nil {
		t.Fatalf("first activate: %v", err)
	}
	if err := activate(); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	active, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || active.State != deploymentStateActive {
		t.Fatalf("after retries: %+v ok=%v err=%v", active, ok, err)
	}
}

func TestDeploymentRacesWebhookUserBuilderAndAgent(t *testing.T) {
	t.Parallel()

	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Race commit", "Ada"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-1")
		errs <- err
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := store.redeployService(ctx, userID, projectID, service.ID)
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
		t.Fatal("expected at least one of webhook enqueue or user redeploy to succeed")
	}

	var currentCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM deployments WHERE service_id = $1 AND is_current = TRUE`, service.ID).Scan(&currentCount); err != nil {
		t.Fatal(err)
	}
	if currentCount != 1 {
		t.Fatalf("expected exactly one current deployment after webhook/user race, got %d", currentCount)
	}

	build, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueue for builder/agent race: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-race", build.ID)
	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("current before builder/agent race: ok=%v err=%v", ok, err)
	}

	var raceWG sync.WaitGroup
	raceErrs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			raceErrs <- store.completeBuild(ctx, "builder-race", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:race", "")
		}()
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			_, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
				AgentId: "node-1",
				Services: []*agentv1.ServiceCondition{{
					AllocationId:             alloc.ID,
					ServiceId:                service.ID,
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
		if err != nil && !errors.Is(err, errBuildNotOwned) {
			t.Fatalf("builder/agent race: %v", err)
		}
	}

	final, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("final current: ok=%v err=%v", ok, err)
	}
	if deploymentStateTerminal(final.State) && final.State != deploymentStateFailed && final.State != deploymentStateSuperseded {
		// terminal is allowed; agent must not have moved a failed/superseded row back to active
	}
	if final.State == deploymentStateFailed || final.State == deploymentStateSuperseded || final.State == deploymentStateCancelled || final.State == deploymentStateCrashed || final.State == deploymentStateRemoved {
		if err := store.withTx(ctx, func(tx *sql.Tx) error {
			_, err := store.applyDeploymentTransitionTx(ctx, tx, final.ID, deploymentTransitionInput{
				ToState:          deploymentStateActive,
				Actor:            deploymentActor{Kind: deploymentCauseAgent, ID: "node-1"},
				ReasonCode:       reasonDeploymentActive,
				Detail:           "post-race resurrect",
				IgnoreIfTerminal: true,
			})
			return err
		}); err != nil {
			t.Fatalf("agent resurrect after race: %v", err)
		}
		after, ok, err := store.currentDeploymentForService(ctx, service.ID)
		if err != nil || !ok || after.State != final.State {
			t.Fatalf("agent changed terminal state from %q to %+v", final.State, after)
		}
	}
}

func setupSourceServiceForDeployment(t *testing.T) (*Store, context.Context, string, string, serviceRecord) {
	t.Helper()
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}

func setupDirectImageServiceForDeployment(t *testing.T) (*Store, context.Context, string, string, serviceRecord) {
	t.Helper()
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}
