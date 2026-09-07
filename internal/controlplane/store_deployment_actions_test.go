//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func pinnedImage(nibble string) string {
	return "registry.example.test/platform/web@sha256:" + strings.Repeat(nibble[:1], 64)
}

func TestDeploymentActionsRestartExactRedeployRollbackRemove(t *testing.T) {
	t.Parallel()
	store, ctx, userID, _, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || first.State != deliverycore.DeploymentStateActive {
		t.Fatalf("first active: %+v ok=%v err=%v", first, ok, err)
	}
	original, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("restart: %v", err)
	} else if action.Action != "restart" || action.ResultDeploymentID == "" {
		t.Fatalf("restart action = %+v", action)
	}
	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("idempotent restart: %v", err)
	} else if action.Action != "restart" {
		t.Fatalf("idempotent restart action = %+v", action)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "restart-all", ""); !errors.Is(err, deliverycore.ErrDeploymentActionConflict) {
		t.Fatalf("idempotency key reuse err = %v", err)
	}
	allocs, err := store.deliveryQueries().ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 2 {
		t.Fatalf("restart should overlap a replacement with the serving allocation: %+v", allocs)
	}
	original = allocationByID(t, store, service.ID, original.ID)
	if original.OperatorRestartNonce != 0 || original.RolloutState != deliverycore.AllocationRolloutServing || !original.Healthy {
		t.Fatalf("restart mutated the serving allocation in place: %+v", original)
	}
	completeActionRollout(t, store, service.ID)

	updatedSpec := directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
		Env:   map[string]string{"STAGE": "two"},
	})
	if _, _, err := updateService(ctx, store, userID, service.ID, "", updatedSpec); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	completeActionRollout(t, store, service.ID)
	second, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || second.State != deliverycore.DeploymentStateActive {
		t.Fatalf("second active: %+v ok=%v err=%v", second, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "stale-restart", ""); !errors.Is(err, deliverycore.ErrDeploymentStale) {
		t.Fatalf("stale restart err = %v", err)
	}

	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "rollback-1", ""); err != nil {
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
	history, err := store.listServiceDeployments(ctx, userID, service.ID, 20)
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
		if rec.ReasonCode == "ROLLBACK" && rec.IsCurrent {
			sawRollback = true
		}
	}
	if !preservedFirst || !sawRollback {
		t.Fatalf("history missing rollback/original: preserved=%v sawRollback=%v", preservedFirst, sawRollback)
	}
	completeActionRollout(t, store, service.ID)
	rolled, ok, err = store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || rolled.State != deliverycore.DeploymentStateActive {
		t.Fatalf("rollback did not become active: %+v ok=%v err=%v", rolled, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, rolled.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "exact-1", ""); err != nil {
		t.Fatalf("exact redeploy: %v", err)
	}
	exact, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("exact current: ok=%v err=%v", ok, err)
	}
	if exact.ImageDigest != pinnedImage("a") || exact.BuildID != rolled.BuildID {
		t.Fatalf("exact redeploy changed snapshot: %+v vs %+v", exact, rolled)
	}
	completeActionRollout(t, store, service.ID)
	active, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, active.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE, "remove-1", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || removed.State != deliverycore.DeploymentStateDraining {
		t.Fatalf("remove should wait for drain: %+v ok=%v err=%v", removed, ok, err)
	}
	allocs, err = store.deliveryQueries().ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) == 0 || allocs[0].RolloutState != deliverycore.AllocationRolloutWithdrawing {
		t.Fatalf("remove did not persist ingress withdrawal before drain: %#v", allocs)
	}
	if _, _, err := testDelivery(store).recordStatusReport(ctx, allocs[0].AgentID, &agentv1.StatusReport{
		AgentId: allocs[0].AgentID,
		Services: []*agentv1.ServiceCondition{{
			AllocationId: allocs[0].ID, ServiceId: service.ID,
			AllocationIpv4:           allocs[0].AllocationIPv4,
			AllocationIpv6:           allocs[0].AllocationIPv6,
			DesiredRolloutGeneration: allocs[0].DesiredRolloutGeneration,
			AppliedRolloutGeneration: allocs[0].DesiredRolloutGeneration,
			Phase:                    "Error", Message: "late runtime response",
		}},
	}); err != nil {
		t.Fatalf("late remove status: %v", err)
	}
	removed, ok, err = store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || removed.State != deliverycore.DeploymentStateDraining || removed.ReasonCode != "USER_REMOVE" {
		t.Fatalf("late agent overwrote remove: %+v ok=%v err=%v", removed, ok, err)
	}
	reconciler := NewRolloutReconciler(newTestDelivery(store, nil, &rolloutIngressProbe{store: store}, nil), time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("begin remove drain: %v", err)
	}
	allocs, err = store.deliveryQueries().ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 || allocs[0].RolloutState != deliverycore.AllocationRolloutDraining || !allocs[0].DrainDeadline.Valid {
		t.Fatalf("remove did not produce graceful drain intent: %+v err=%v", allocs, err)
	}
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("finish remove drain: %v", err)
	}
	removed, ok, err = store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || removed.State != deliverycore.DeploymentStateRemoved {
		t.Fatalf("removed current: %+v ok=%v err=%v", removed, ok, err)
	}
	allocs, err = store.deliveryQueries().ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) != 0 {
		t.Fatalf("expected drained allocations to be withdrawn, got %#v err=%v", allocs, err)
	}
	history, err = store.listServiceDeployments(ctx, userID, service.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("remove discarded deployment history")
	}
}

func TestDeploymentActionRestartReplacesOnlySelectedAllocation(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 2, 1)
	ctx := context.Background()
	before := mustRolloutAllocations(t, store, service.ID)
	if len(before) != 2 {
		t.Fatalf("initial allocations = %+v", before)
	}
	selected, unaffected := before[0], before[1]
	target, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || target.State != deliverycore.DeploymentStateActive {
		t.Fatalf("active deployment: %+v ok=%v err=%v", target, ok, err)
	}

	if _, action, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, target.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-selected", selected.ID); err != nil {
		t.Fatalf("restart selected allocation: %v", err)
	} else if action.ResultDeploymentID == "" || action.AllocationID != selected.ID {
		t.Fatalf("restart action = %+v", action)
	}
	during := mustRolloutAllocations(t, store, service.ID)
	if len(during) != 3 {
		t.Fatalf("selected restart should add one surge allocation: %+v", during)
	}
	if got := allocationByID(t, store, service.ID, unaffected.ID); got.RolloutState != deliverycore.AllocationRolloutServing || !got.Healthy {
		t.Fatalf("unselected allocation changed during restart: %+v", got)
	}
	if got := allocationByID(t, store, service.ID, selected.ID); got.RolloutState != deliverycore.AllocationRolloutServing || !got.Healthy {
		t.Fatalf("selected allocation stopped before its replacement was ready: %+v", got)
	}

	completeActionRollout(t, store, service.ID)
	after := mustRolloutAllocations(t, store, service.ID)
	if len(after) != 2 {
		t.Fatalf("selected restart changed replica count: %+v", after)
	}
	if got := allocationByID(t, store, service.ID, unaffected.ID); got.ID != unaffected.ID || !got.Healthy {
		t.Fatalf("unselected allocation was replaced: %+v", got)
	}
	for _, alloc := range after {
		if alloc.ID == selected.ID {
			t.Fatalf("selected allocation survived rolling restart: %+v", after)
		}
	}
}

func TestDeploymentActionCancelIgnoresLateBuilderAndAgent(t *testing.T) {
	t.Parallel()
	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-cancel", "Cancel me", "Ada"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-cancel")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-cancel", build.ID)
	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || current.State != deliverycore.DeploymentStateBuilding {
		t.Fatalf("building: %+v ok=%v err=%v", current, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-1", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-1", ""); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if err := completeBuildForTest(ctx, store, "builder-cancel", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-cancel", pinnedImage("c"), ""); err != nil {
		t.Fatalf("late completeBuild: %v", err)
	}
	after, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("after late complete: ok=%v err=%v", ok, err)
	}
	cancelled := deploymentByIDForTest(t, store, ctx, userID, projectID, service.ID, current.ID)
	if cancelled.State != deliverycore.DeploymentStateCancelled {
		t.Fatalf("late builder overwrote cancel: %+v current=%+v", cancelled, after)
	}

	alloc, err := store.allocationByServiceID(ctx, service.ID)
	if err == nil {
		if _, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
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
	if still.State != deliverycore.DeploymentStateCancelled {
		t.Fatalf("late agent overwrote cancel: %+v", still)
	}
}

func TestDeploymentActionRetryCancelledUnresolvedSource(t *testing.T) {
	t.Parallel()
	store, ctx, userID, projectID, service := setupSourceServiceForDeployment(t)
	staged, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || staged.State != deliverycore.DeploymentStateStaged || staged.BuildID != "" || staged.ImageDigest != "" {
		t.Fatalf("initial staged deployment: %+v ok=%v err=%v", staged, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, staged.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-unresolved", ""); err != nil {
		t.Fatalf("cancel unresolved deployment: %v", err)
	}
	assertRolloutState(t, store, service.ID, 1, "superseded", "cancelled by user")

	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, staged.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-unresolved", ""); err != nil {
		t.Fatalf("retry unresolved deployment: %v", err)
	} else if action.ResultDeploymentID == "" {
		t.Fatalf("retry did not create a staged deployment: %+v", action)
	}
	retried, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("retried deployment: ok=%v err=%v", ok, err)
	}
	if retried.State != deliverycore.DeploymentStateStaged || retried.ReasonCode != "USER_RETRY" || retried.RolloutGeneration != 2 {
		t.Fatalf("retried deployment = %+v, want staged rollout 2", retried)
	}
	if retried.ResolvedSpec == nil || deliverycore.DesiredSourceSpec(retried.ResolvedSpec) == nil {
		t.Fatalf("retried deployment lost its source snapshot: %+v", retried)
	}
	assertRolloutState(t, store, service.ID, 2, "pending_build", "")
	var queued int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM source_work_items
		  WHERE service_id = $1 AND spec_revision = $2 AND state = $3`,
		service.ID, retried.SpecRevision, deliverycore.SourceWorkStatePending,
	).Scan(&queued); err != nil {
		t.Fatalf("count retry source work: %v", err)
	}
	if queued != 1 {
		t.Fatalf("retry source work count = %d, want 1", queued)
	}
	current := deploymentByIDForTest(t, store, ctx, userID, projectID, service.ID, retried.ID)
	if !current.IsCurrent {
		t.Fatalf("retried deployment is not current: %+v", current)
	}
}

func TestDeleteServiceAfterCancelledDeployment(t *testing.T) {
	t.Parallel()
	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	staged, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("staged deployment: ok=%v err=%v", ok, err)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, staged.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-before-delete", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := deleteService(ctx, store, userID, service.ID); err != nil {
		t.Fatalf("delete cancelled service: %v", err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM services WHERE id = $1`, service.ID).Scan(&remaining); err != nil {
		t.Fatalf("count deleted service: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("cancelled service still exists: count=%d", remaining)
	}
}

func TestDeploymentActionCancelDeployingRestoresServingGeneration(t *testing.T) {
	store, ctx, userID, projectID, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	serving := mustRolloutAllocations(t, store, service.ID)[0]
	updated := directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})})
	if _, _, err := updateService(ctx, store, userID, service.ID, "", updated); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, service.EnvironmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	cancelledTarget, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || !deploymentStatePreActive(cancelledTarget.State) {
		t.Fatalf("deploying target: %+v ok=%v err=%v", cancelledTarget, ok, err)
	}

	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, cancelledTarget.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-deploying", ""); err != nil {
		t.Fatalf("cancel deploying rollout: %v", err)
	} else if action.ResultDeploymentID == "" {
		t.Fatalf("cancel did not create fallback rollout: %+v", action)
	}
	if got := deploymentByIDForTest(t, store, ctx, userID, projectID, service.ID, cancelledTarget.ID); got.State != deliverycore.DeploymentStateCancelled {
		t.Fatalf("cancelled deployment was overwritten: %+v", got)
	}
	if got := allocationByID(t, store, service.ID, serving.ID); got.RolloutState != deliverycore.AllocationRolloutServing || !got.Healthy {
		t.Fatalf("cancel stopped the serving predecessor: %+v", got)
	}
	restored, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || restored.ImageDigest != pinnedImage("a") {
		t.Fatalf("fallback deployment: %+v ok=%v err=%v", restored, ok, err)
	}
	completeActionRollout(t, store, service.ID)
}

func TestDeploymentActionCancelWithoutReusableFallbackDrainsServingAllocations(t *testing.T) {
	store, ctx, userID, _, service := setupPinnedImageServiceForDeployment(t, "nginx:latest")
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	serving := mustRolloutAllocations(t, store, service.ID)[0]
	updated := directImageServiceSpec("nginx:edge", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})})
	if _, _, err := updateService(ctx, store, userID, service.ID, "", updated); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, service.EnvironmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	cancelledTarget, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || !deploymentStatePreActive(cancelledTarget.State) {
		t.Fatalf("deploying target: %+v ok=%v err=%v", cancelledTarget, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, cancelledTarget.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-unpinned", ""); err != nil {
		t.Fatalf("cancel deploying rollout: %v", err)
	}
	got := allocationByID(t, store, service.ID, serving.ID)
	if got.ID != serving.ID || got.RolloutState != deliverycore.AllocationRolloutWithdrawing {
		t.Fatalf("cancel deleted serving history instead of withdrawing it: %+v", got)
	}
	for _, alloc := range mustListAllocations(t, store, ctx, service.ID) {
		if alloc.DesiredRolloutGeneration == cancelledTarget.RolloutGeneration && alloc.RolloutState == deliverycore.AllocationRolloutStarting {
			t.Fatalf("cancelled starting allocation survived: %+v", alloc)
		}
	}
}

func TestDeploymentActionRetryAndConcurrentIdempotency(t *testing.T) {
	t.Parallel()
	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-retry", "Retry me", "Ada"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-retry")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-retry", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-retry", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-retry", "", "boom"); err != nil {
		t.Fatal(err)
	}
	failed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || failed.State != deliverycore.DeploymentStateFailed {
		t.Fatalf("failed: %+v ok=%v err=%v", failed, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, "missing-id", platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-stale", ""); err == nil {
		t.Fatal("expected missing deployment to fail")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, failed.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-same", "")
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
	if failedCount != 0 {
		t.Fatalf("%d concurrent identical retries failed", failedCount)
	}
	retried, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatalf("retry current: ok=%v err=%v", ok, err)
	}
	if retried.ID == failed.ID {
		t.Fatal("retry did not create a new deployment")
	}
	history, err := store.listServiceDeployments(ctx, userID, service.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawRetryAction bool
	for _, rec := range history {
		if rec.ID == failed.ID {
			for _, action := range rec.Actions {
				if action.Action == "retry" {
					sawRetryAction = true
				}
			}
		}
	}
	if !sawRetryAction {
		t.Fatal("retry is not visible in deployment history")
	}
}

func deploymentByIDForTest(t *testing.T, store *Store, ctx context.Context, userID, projectID, serviceID, deploymentID string) deliverycore.DeploymentRecord {
	t.Helper()
	history, err := store.listServiceDeployments(ctx, userID, serviceID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range history {
		if rec.ID == deploymentID {
			return rec
		}
	}
	t.Fatalf("deployment %s not found", deploymentID)
	return deliverycore.DeploymentRecord{}
}

func setupPinnedImageServiceForDeployment(t *testing.T, image string) (*Store, context.Context, string, string, deliverycore.ServiceRecord) {
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
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec(image, &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
		Env:   map[string]string{"STAGE": "one"},
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, ctx, "user-1", projects[0].ID, service
}

func completeActionRollout(t *testing.T, store *Store, serviceID string) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 10; attempt++ {
		var generation int64
		var state string
		if err := store.db.QueryRowContext(ctx,
			`SELECT s.current_rollout_generation, sr.state
			   FROM services s
			   JOIN service_rollouts sr ON sr.service_id = s.id AND sr.rollout_generation = s.current_rollout_generation
			  WHERE s.id = $1`, serviceID,
		).Scan(&generation, &state); err != nil {
			t.Fatal(err)
		}
		if state == "succeeded" {
			return
		}
		for _, alloc := range allocationForGeneration(t, store, serviceID, generation) {
			if alloc.RolloutState == deliverycore.AllocationRolloutStarting {
				markRolloutAllocationReady(t, store, alloc)
			}
		}
		reconciler := NewRolloutReconciler(newTestDelivery(store, nil, &rolloutIngressProbe{store: store}, nil), time.Second)
		if err := reconciler.Reconcile(ctx); err != nil {
			t.Fatalf("advance action rollout: %v", err)
		}
		markAllDrainingComplete(t, store, serviceID)
	}
	t.Fatal("action rollout did not complete")
}
