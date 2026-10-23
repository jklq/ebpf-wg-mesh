//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func serverlessStateForTest(t *testing.T, store *persistence, ctx context.Context, serviceID string) string {
	t.Helper()
	var state string
	err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT state FROM service_serverless_state WHERE service_id = $1), '')`, serviceID).Scan(&state)
	if err != nil {
		t.Fatalf("serverless state: %v", err)
	}
	return state
}

func assignmentIntentForTest(t *testing.T, store *persistence, ctx context.Context, serviceID string) (intent, rolloutState string) {
	t.Helper()
	err := store.db.QueryRowContext(ctx,
		`SELECT intent, rollout_state FROM allocation_assignments WHERE service_id = $1 ORDER BY id LIMIT 1`, serviceID).
		Scan(&intent, &rolloutState)
	if err != nil {
		t.Fatalf("assignment intent: %v", err)
	}
	return intent, rolloutState
}

func TestServerlessSleepAndWake(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})

	spec := replicaSpec(100, 64)
	enabled := true
	spec.ServerlessEnabled = &enabled
	service, err := createService(ctx, store, "user-1", envID, "web", spec, "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 1)
	if err := reportActiveForTest(ctx, store, service.ID); err != nil {
		t.Fatalf("reportActiveForTest: %v", err)
	}
	d := testDelivery(store)
	if err := d.ReconcileRollouts(ctx); err != nil {
		t.Fatalf("ReconcileRollouts: %v", err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 1 || !deliverycore.AllocationReady(allocations[0]) {
		t.Fatalf("expected one ready allocation before sleep, got %+v", allocations)
	}

	// Backdate activity past the idle timeout, then evaluate.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO service_serverless_state(service_id, state, last_activity_at, updated_at)
		VALUES ($1, 'awake', now() - interval '30 minutes', now())`, service.ID); err != nil {
		t.Fatalf("seed serverless activity: %v", err)
	}
	if err := d.Delivery.EvaluateServerlessIdle(ctx, 10*time.Minute); err != nil {
		t.Fatalf("EvaluateServerlessIdle(sleep): %v", err)
	}
	if got := serverlessStateForTest(t, store, ctx, service.ID); got != deliverycore.ServerlessAsleep {
		t.Fatalf("expected asleep after idle evaluation, got %q", got)
	}
	intent, rolloutState := assignmentIntentForTest(t, store, ctx, service.ID)
	if intent != "sleep" || rolloutState != deliverycore.AllocationRolloutSleeping {
		t.Fatalf("expected sleep intent/sleeping assignment, got intent=%q rollout_state=%q", intent, rolloutState)
	}
	snapshot, err := store.reads.ServiceSnapshot(ctx, service.ID)
	if err != nil {
		t.Fatalf("ServiceSnapshot: %v", err)
	}
	if snapshot.ServerlessState != deliverycore.ServerlessAsleep {
		t.Fatalf("expected snapshot asleep, got %q", snapshot.ServerlessState)
	}
	if snapshot.DesiredReplicaCount != 1 {
		t.Fatalf("sleep must preserve desired replicas, got %d", snapshot.DesiredReplicaCount)
	}
	// The assignment is retained: same allocation, same addresses, and the
	// agent sees a SLEEP intent for it.
	afterSleep := mustListAllocations(t, store, ctx, service.ID)
	if len(afterSleep) != 1 || afterSleep[0].ID != allocations[0].ID ||
		afterSleep[0].AllocationIPv4 != allocations[0].AllocationIPv4 {
		t.Fatalf("sleep must retain the assignment, before=%+v after=%+v", allocations, afterSleep)
	}
	desired, err := desiredStateForAgent(ctx, store, "node-a")
	if err != nil {
		t.Fatalf("DesiredStateForAgent: %v", err)
	}
	foundSleepIntent := false
	for _, svc := range desired.GetServices() {
		if svc.GetServiceId() == service.ID && svc.GetIntent() == agentv1.AllocationIntent_ALLOCATION_INTENT_SLEEP {
			foundSleepIntent = true
		}
	}
	if !foundSleepIntent {
		t.Fatal("agent desired state does not carry SLEEP intent for the sleeping service")
	}

	// Wake: state becomes waking and the assignment runs again.
	woke, err := d.Delivery.WakeService(ctx, service.ID, deliverycore.WakeTriggerOperator)
	if err != nil || !woke {
		t.Fatalf("WakeService: woke=%v err=%v", woke, err)
	}
	if got := serverlessStateForTest(t, store, ctx, service.ID); got != deliverycore.ServerlessWaking {
		t.Fatalf("expected waking after wake, got %q", got)
	}
	intent, rolloutState = assignmentIntentForTest(t, store, ctx, service.ID)
	if intent != "run" || rolloutState != deliverycore.AllocationRolloutServing {
		t.Fatalf("expected run intent/serving after wake, got intent=%q rollout_state=%q", intent, rolloutState)
	}

	// Readiness promotes waking to awake.
	if err := reportActiveForTest(ctx, store, service.ID); err != nil {
		t.Fatalf("reportActiveForTest(after wake): %v", err)
	}
	if err := d.ReconcileRollouts(ctx); err != nil {
		t.Fatalf("ReconcileRollouts(after wake): %v", err)
	}
	if err := d.Delivery.EvaluateServerlessIdle(ctx, 10*time.Minute); err != nil {
		t.Fatalf("EvaluateServerlessIdle(promote): %v", err)
	}
	if got := serverlessStateForTest(t, store, ctx, service.ID); got != deliverycore.ServerlessAwake {
		t.Fatalf("expected awake after readiness, got %q", got)
	}

	// A plain service in the same environment is never tracked or slept.
	plain, err := createService(ctx, store, "user-1", envID, "api", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService(plain): %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, plain.ID, 1)
	if err := d.Delivery.EvaluateServerlessIdle(ctx, time.Nanosecond); err != nil {
		t.Fatalf("EvaluateServerlessIdle(plain): %v", err)
	}
	if got := serverlessStateForTest(t, store, ctx, plain.ID); got != "" {
		t.Fatalf("non-serverless service must not be tracked, got state %q", got)
	}
}

func TestServerlessWakeByHostnameWaitsForReadiness(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})

	spec := replicaSpec(100, 64)
	enabled := true
	spec.ServerlessEnabled = &enabled
	service, err := createService(ctx, store, "user-1", envID, "web", spec, "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 1)
	if err := reportActiveForTest(ctx, store, service.ID); err != nil {
		t.Fatalf("reportActiveForTest: %v", err)
	}
	d := testDelivery(store)
	if err := d.ReconcileRollouts(ctx); err != nil {
		t.Fatalf("ReconcileRollouts: %v", err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 1 {
		t.Fatalf("allocations = %+v", allocations)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO domain_bindings(hostname, service_id, target_port, created_at, updated_at)
		VALUES ('wake.example.com', $1, 8080, now(), now())`, service.ID); err != nil {
		t.Fatalf("seed domain binding: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO service_serverless_state(service_id, state, last_activity_at, updated_at)
		VALUES ($1, 'awake', now() - interval '30 minutes', now())`, service.ID); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	if err := d.Delivery.EvaluateServerlessIdle(ctx, 10*time.Minute); err != nil {
		t.Fatalf("EvaluateServerlessIdle: %v", err)
	}
	if got := serverlessStateForTest(t, store, ctx, service.ID); got != deliverycore.ServerlessAsleep {
		t.Fatalf("expected asleep, got %q", got)
	}
	// Report the sleeping observation a real agent sends, so the live
	// overlay no longer considers the allocation healthy.
	for _, alloc := range allocations {
		report := &agentv1.StatusReport{AgentId: alloc.AgentID, Services: []*agentv1.ServiceCondition{{
			AllocationId: alloc.ID, ServiceId: service.ID,
			DesiredRolloutGeneration: alloc.DesiredRolloutGeneration,
			AppliedSpecRevision:      alloc.DesiredSpecRevision,
			AppliedRolloutGeneration: alloc.DesiredRolloutGeneration,
			Phase:                    "Sleeping",
			Healthy:                  false,
			AllocationIpv4:           alloc.AllocationIPv4,
			AllocationIpv6:           alloc.AllocationIPv6,
		}}}
		if _, _, err := d.recordStatusReport(ctx, alloc.AgentID, report); err != nil {
			t.Fatalf("recordStatusReport(sleeping): %v", err)
		}
	}

	// Unknown hostnames never resolve to a service.
	if _, err := d.Delivery.WakeServiceByHostname(ctx, "missing.example.com"); err != deliverycore.ErrWakeHostnameNotFound {
		t.Fatalf("unknown hostname err = %v", err)
	}

	// The wake blocks until the allocation reports ready, then returns its
	// retained endpoint for the activator to proxy the held request to.
	wakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type wakeOutcome struct {
		result deliverycore.WakeResult
		err    error
	}
	outcome := make(chan wakeOutcome, 1)
	go func() {
		result, err := d.Delivery.WakeServiceByHostname(wakeCtx, "Wake.Example.com")
		outcome <- wakeOutcome{result, err}
	}()
	// Let the wake flip intents, then report the allocation healthy.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := serverlessStateForTest(t, store, ctx, service.ID); got == deliverycore.ServerlessWaking {
			break
		}
		if time.Now().After(deadline) {
			intent, rolloutState := assignmentIntentForTest(t, store, ctx, service.ID)
			t.Fatalf("wake did not start: state=%q intent=%q rollout_state=%q",
				serverlessStateForTest(t, store, ctx, service.ID), intent, rolloutState)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := reportActiveForTest(ctx, store, service.ID); err != nil {
		t.Fatalf("reportActiveForTest(after wake): %v", err)
	}
	if err := d.ReconcileRollouts(ctx); err != nil {
		t.Fatalf("ReconcileRollouts(after wake): %v", err)
	}
	got := <-outcome
	if got.err != nil {
		t.Fatalf("WakeServiceByHostname: %v", got.err)
	}
	if !got.result.Ready || got.result.ServiceID != service.ID || len(got.result.Endpoints) != 1 {
		t.Fatalf("wake result = %+v", got.result)
	}
	if got.result.Endpoints[0].Address != allocations[0].AllocationIPv4 || got.result.Endpoints[0].Port != 8080 {
		t.Fatalf("endpoint = %+v, want %s:8080", got.result.Endpoints[0], allocations[0].AllocationIPv4)
	}
	if got := serverlessStateForTest(t, store, ctx, service.ID); got != deliverycore.ServerlessAwake {
		t.Fatalf("expected awake after successful wake, got %q", got)
	}
}
