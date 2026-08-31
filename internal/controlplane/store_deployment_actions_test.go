//go:build integration

package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func pinnedImage(nibble string) string {
	return "registry.example.test/platform/web@sha256:" + strings.Repeat(nibble[:1], 64)
}

func TestDeploymentActionsRestartExactRedeployRollbackRemove(t *testing.T) {
	t.Parallel()
	store, ctx, userID, projectID, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || first.State != deploymentStateActive {
		t.Fatalf("first active: %+v ok=%v err=%v", first, ok, err)
	}

	if _, action, err := store.applyDeploymentAction(ctx, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("restart: %v", err)
	} else if action.Action != deploymentActionRestart || action.ResultDeploymentID != "" {
		t.Fatalf("restart action = %+v", action)
	}
	if _, action, err := store.applyDeploymentAction(ctx, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("idempotent restart: %v", err)
	} else if action.Action != deploymentActionRestart {
		t.Fatalf("idempotent restart action = %+v", action)
	}
	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if alloc.OperatorRestartNonce != 1 {
		t.Fatalf("restart nonce = %d", alloc.OperatorRestartNonce)
	}

	updatedSpec := directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
		Env:   map[string]string{"STAGE": "two"},
	})
	if _, _, err := store.updateService(ctx, userID, projectID, service.ID, "", updatedSpec); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, userID, projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	second, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || second.State != deploymentStateActive {
		t.Fatalf("second active: %+v ok=%v err=%v", second, ok, err)
	}

	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "stale-restart", ""); !errors.Is(err, errDeploymentStale) {
		t.Fatalf("stale restart err = %v", err)
	}

	if _, action, err := store.applyDeploymentAction(ctx, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "rollback-1", ""); err != nil {
		t.Fatalf("rollback: %v", err)
	} else if action.ResultDeploymentID == "" || action.ResultDeploymentID == first.ID {
		t.Fatalf("rollback should create a new deployment, got %+v", action)
	}
	rolled, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("rollback current: ok=%v err=%v", ok, err)
	}
	if rolled.ImageDigest != pinnedImage("a") {
		t.Fatalf("rollback image = %q", rolled.ImageDigest)
	}
	if rolled.ID == first.ID {
		t.Fatal("rollback reused the historical deployment row")
	}
	history, err := store.listServiceDeployments(ctx, userID, projectID, service.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawRollback, preservedFirst bool
	for _, rec := range history {
		if rec.ID == first.ID {
			preservedFirst = true
			if len(rec.Actions) == 0 {
				t.Fatal("rollback is not visible on the historical deployment")
			}
		}
		if rec.ReasonCode == reasonRollback && rec.IsCurrent {
			sawRollback = true
		}
	}
	if !preservedFirst || !sawRollback {
		t.Fatalf("history missing rollback/original: preserved=%v sawRollback=%v", preservedFirst, sawRollback)
	}

	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, rolled.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "exact-1", ""); err != nil {
		t.Fatalf("exact redeploy: %v", err)
	}
	exact, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("exact current: ok=%v err=%v", ok, err)
	}
	if exact.ImageDigest != pinnedImage("a") || exact.BuildID != rolled.BuildID {
		t.Fatalf("exact redeploy changed snapshot: %+v vs %+v", exact, rolled)
	}

	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	active, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, active.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE, "remove-1", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || removed.State != deploymentStateRemoved {
		t.Fatalf("removed current: %+v ok=%v err=%v", removed, ok, err)
	}
	allocs, err := store.listAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 0 {
		t.Fatalf("expected allocations to be withdrawn, got %#v", allocs)
	}
	history, err = store.listServiceDeployments(ctx, userID, projectID, service.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("remove discarded deployment history")
	}
}

func TestDeploymentActionCancelIgnoresLateBuilderAndAgent(t *testing.T) {
	t.Parallel()
	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-cancel", "Cancel me", "Ada"); err != nil {
		t.Fatal(err)
	}
	build, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-cancel")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-cancel", build.ID)
	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || current.State != deploymentStateBuilding {
		t.Fatalf("building: %+v ok=%v err=%v", current, ok, err)
	}

	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-1", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-1", ""); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if err := store.completeBuild(ctx, "builder-cancel", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-cancel", pinnedImage("c"), ""); err != nil {
		t.Fatalf("late completeBuild: %v", err)
	}
	after, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("after late complete: ok=%v err=%v", ok, err)
	}
	cancelled := deploymentByIDForTest(t, store, ctx, userID, projectID, service.ID, current.ID)
	if cancelled.State != deploymentStateCancelled {
		t.Fatalf("late builder overwrote cancel: %+v current=%+v", cancelled, after)
	}

	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err == nil {
		if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
			AgentId: "node-1",
			Services: []*agentv1.ServiceCondition{{
				AllocationId:             alloc.ID,
				ServiceId:                service.ID,
				DesiredRolloutGeneration: current.RolloutGeneration,
				AppliedRolloutGeneration: current.RolloutGeneration,
				Phase:                    "Healthy",
				Healthy:                  true,
			}},
		}); err != nil {
			t.Fatalf("late agent: %v", err)
		}
	}
	still := deploymentByIDForTest(t, store, ctx, userID, projectID, service.ID, current.ID)
	if still.State != deploymentStateCancelled {
		t.Fatalf("late agent overwrote cancel: %+v", still)
	}
}

func TestDeploymentActionRetryAndConcurrentIdempotency(t *testing.T) {
	t.Parallel()
	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-retry", "Retry me", "Ada"); err != nil {
		t.Fatal(err)
	}
	build, err := store.enqueueBuildForService(ctx, userID, projectID, service.ID, "commit-retry")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-retry", build.ID)
	if err := store.completeBuild(ctx, "builder-retry", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-retry", "", "boom"); err != nil {
		t.Fatal(err)
	}
	failed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || failed.State != deploymentStateFailed {
		t.Fatalf("failed: %+v ok=%v err=%v", failed, ok, err)
	}

	if _, _, err := store.applyDeploymentAction(ctx, userID, service.ID, "missing-id", platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-stale", ""); err == nil {
		t.Fatal("expected missing deployment to fail")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := store.applyDeploymentAction(ctx, userID, service.ID, failed.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-same", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	var failedCount int
	for err := range errs {
		if err != nil {
			failedCount++
		}
	}
	if failedCount == 8 {
		t.Fatal("concurrent identical retries all failed")
	}
	retried, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("retry current: ok=%v err=%v", ok, err)
	}
	if retried.ID == failed.ID {
		t.Fatal("retry did not create a new deployment")
	}
	history, err := store.listServiceDeployments(ctx, userID, projectID, service.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawRetryAction bool
	for _, rec := range history {
		if rec.ID == failed.ID {
			for _, action := range rec.Actions {
				if action.Action == deploymentActionRetry {
					sawRetryAction = true
				}
			}
		}
	}
	if !sawRetryAction {
		t.Fatal("retry is not visible in deployment history")
	}
}

func deploymentByIDForTest(t *testing.T, store *Store, ctx context.Context, userID, projectID, serviceID, deploymentID string) deploymentRecord {
	t.Helper()
	history, err := store.listServiceDeployments(ctx, userID, projectID, serviceID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range history {
		if rec.ID == deploymentID {
			return rec
		}
	}
	t.Fatalf("deployment %s not found", deploymentID)
	return deploymentRecord{}
}

func setupPinnedImageServiceForDeployment(t *testing.T, image string) (*Store, context.Context, string, string, serviceRecord) {
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec(image, &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
		Env:   map[string]string{"STAGE": "one"},
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}
