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
	store, ctx, userID, _, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || first.State != deploymentStateActive {
		t.Fatalf("first active: %+v ok=%v err=%v", first, ok, err)
	}

	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("restart: %v", err)
	} else if action.Action != deploymentActionRestart || action.ResultDeploymentID != "" {
		t.Fatalf("restart action = %+v", action)
	}
	if _, action, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("idempotent restart: %v", err)
	} else if action.Action != deploymentActionRestart {
		t.Fatalf("idempotent restart action = %+v", action)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "restart-all", ""); !errors.Is(err, errDeploymentActionConflict) {
		t.Fatalf("idempotency key reuse err = %v", err)
	}
	allocs, err := store.listAllocationsByServiceID(ctx, service.ID)
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
	if _, _, err := updateService(ctx, store, userID, service.ID, "", updatedSpec); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	second, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || second.State != deploymentStateActive {
		t.Fatalf("second active: %+v ok=%v err=%v", second, ok, err)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "stale-restart", ""); !errors.Is(err, errDeploymentStale) {
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
		if rec.ReasonCode == reasonRollback && rec.IsCurrent {
			sawRollback = true
		}
	}
	if !preservedFirst || !sawRollback {
		t.Fatalf("history missing rollback/original: preserved=%v sawRollback=%v", preservedFirst, sawRollback)
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

	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.11", 8081); err != nil {
		t.Fatal(err)
	}
	active, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, active.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE, "remove-1", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removed, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || removed.State != deploymentStateDraining {
		t.Fatalf("remove should wait for drain: %+v ok=%v err=%v", removed, ok, err)
	}
	allocs, err = store.listAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) == 0 || allocs[0].RolloutState != allocationRolloutWithdrawing {
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
	if err != nil || !ok || removed.State != deploymentStateDraining || removed.ReasonCode != reasonUserRemove {
		t.Fatalf("late agent overwrote remove: %+v ok=%v err=%v", removed, ok, err)
	}
	reconciler := NewRolloutReconciler(NewDelivery(store, nil, &rolloutIngressProbe{store: store}, nil), time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("begin remove drain: %v", err)
	}
	allocs, err = store.listAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 || allocs[0].RolloutState != allocationRolloutDraining || !allocs[0].DrainDeadline.Valid {
		t.Fatalf("remove did not produce graceful drain intent: %+v err=%v", allocs, err)
	}
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("finish remove drain: %v", err)
	}
	removed, ok, err = store.currentDeploymentForService(ctx, service.ID)
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
	history, err = store.listServiceDeployments(ctx, userID, service.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("remove discarded deployment history")
	}
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
		if state == rolloutStateSucceeded {
			return
		}
		for _, alloc := range allocationForGeneration(t, store, serviceID, generation) {
			if alloc.RolloutState == allocationRolloutStarting {
				markRolloutAllocationReady(t, store, alloc)
			}
		}
		reconciler := NewRolloutReconciler(NewDelivery(store, nil, &rolloutIngressProbe{store: store}, nil), time.Second)
		if err := reconciler.Reconcile(ctx); err != nil {
			t.Fatalf("advance action rollout: %v", err)
		}
		markAllDrainingComplete(t, store, serviceID)
	}
	t.Fatal("action rollout did not complete")
}
