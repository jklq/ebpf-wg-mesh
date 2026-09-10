//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/restartpolicy"
)

func TestRecordStatusReportPersistsCrashLoopAndWithdrawsIngress(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		Ports:           runtimePortsFromInts([]int32{8080}),
		CpuMillis:       250,
		MemoryMebibytes: 128,
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "web.example.test", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}
	backends, err := store.routing.HealthyIngressBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 1 {
		t.Fatalf("expected healthy backend, got %#v", backends)
	}
	_, allocs, err := store.reads.ServiceStatus(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}

	report := &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			AllocationIpv4:           alloc.AllocationIPv4,
			AllocationIpv6:           alloc.AllocationIPv6,
			AppliedSpecRevision:      1,
			AppliedRolloutGeneration: 1,
			Phase:                    restartpolicy.PhaseCrashLoop,
			Message:                  "crash loop after non-zero exit",
			HealthyIpv6Ports:         []int32{8080},
			Healthy:                  false,
			Restart: &platformv1.RestartObservation{
				RestartCount:             1,
				CrashLoop:                true,
				AppliedRolloutGeneration: 1,
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
			},
		}},
	}
	ingress := &countingIngress{}
	delivery := newTestDelivery(store, nil, ingress, NewPlatformEvents(store.events, time.Millisecond))
	prepareTestStatusReport(ctx, store, "node-1", report)
	beforeReportRevision, err := store.events.currentGlobalRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = delivery.ObserveAgentStatus(ctx, "node-1", report)
	if err != nil {
		t.Fatalf("ObserveAgentStatus: %v", err)
	}
	if ingress.requests.Load() != 1 {
		t.Fatalf("expected one ingress request, got %d", ingress.requests.Load())
	}
	// Recording the transient sample and applying the resulting durable
	// deployment decision are intentionally separate transactions.
	if got, err := store.events.currentGlobalRevision(ctx); err != nil || got != beforeReportRevision+2 {
		t.Fatalf("global revision after status report = %d, %v", got, err)
	}
	updated, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Phase != restartpolicy.PhaseCrashLoop || updated.Healthy || !updated.Restart.GetCrashLoop() {
		t.Fatalf("allocation not persisted as crash-loop: %+v", updated)
	}
	backends, err = store.routing.HealthyIngressBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 0 {
		t.Fatalf("crash-loop allocation still in ingress: %#v", backends)
	}
}

func TestDeploymentActionsRestartAndExactRedeployResetObservation(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		Ports:           runtimePortsFromInts([]int32{8080}),
		CpuMillis:       250,
		MemoryMebibytes: 128,
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}
	current := currentDeploymentForTest(t, store, ctx, service.ID)
	if current.State != deliverycore.DeploymentStateActive {
		t.Fatalf("current deployment: %+v", current)
	}
	original, err := store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-all", ""); err != nil {
		t.Fatalf("applyDeploymentAction(RESTART): %v", err)
	}
	afterRestart := mustListAllocations(t, store, ctx, service.ID)
	if len(afterRestart) != 2 {
		t.Fatalf("rolling restart should overlap a replacement: %+v", afterRestart)
	}
	kept := allocationByID(t, store, service.ID, original.ID)
	if kept.ID != original.ID || kept.OperatorRestartNonce != 0 || kept.RolloutState != deliverycore.AllocationRolloutServing || !kept.Healthy {
		t.Fatalf("restart mutated the serving allocation in place: %+v", kept)
	}
	var replacement deliverycore.AllocationRecord
	for _, alloc := range afterRestart {
		if alloc.ID != original.ID {
			replacement = alloc
			break
		}
	}
	if replacement.ID == "" || replacement.RolloutState != deliverycore.AllocationRolloutStarting {
		t.Fatalf("expected a starting replacement allocation, got %+v", afterRestart)
	}

	desired, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.GetServices()) != 2 {
		t.Fatalf("desired state missing overlapping restart allocations: %#v", desired.GetServices())
	}

	completeActionRollout(t, store, service.ID)
	serving := mustRolloutAllocations(t, store, service.ID)
	if len(serving) != 1 {
		t.Fatalf("completed restart allocations = %+v", serving)
	}
	if serving[0].ID == original.ID {
		t.Fatal("original allocation survived rolling restart")
	}
	if _, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		Services: []*agentv1.ServiceCondition{{
			AllocationId: serving[0].ID, ServiceId: service.ID,
			AllocationIpv4: serving[0].AllocationIPv4, AllocationIpv6: serving[0].AllocationIPv6,
			Phase: restartpolicy.PhaseCrashLoop, Message: "crash loop",
			Restart: &platformv1.RestartObservation{CrashLoop: true, RestartCount: 3, AppliedRolloutGeneration: serving[0].DesiredRolloutGeneration},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	crashed := currentDeploymentForTest(t, store, ctx, service.ID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, crashed.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "clear-crash-loop", ""); err != nil {
		t.Fatalf("applyDeploymentAction(EXACT_REDEPLOY): %v", err)
	}
	afterRollout := allocationForGeneration(t, store, service.ID, serving[0].DesiredRolloutGeneration+1)
	if len(afterRollout) != 1 {
		t.Fatalf("new rollout allocation = %+v", afterRollout)
	}
	if afterRollout.Restart.GetCrashLoop() {
		t.Fatal("new rollout left crash-loop observation in place")
	}
}

func TestDesiredStateCarriesPersistedRestartObservation(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	_, allocs, err := store.reads.ServiceStatus(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	alloc := primaryAllocation(allocs)
	if _, _, err := testDelivery(store).recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		Services: []*agentv1.ServiceCondition{{
			AllocationId: alloc.ID, ServiceId: service.ID,
			AllocationIpv4: alloc.AllocationIPv4, AllocationIpv6: alloc.AllocationIPv6,
			AppliedSpecRevision: 1, AppliedRolloutGeneration: 1,
			Phase: restartpolicy.PhaseCrashLoop,
			Restart: &platformv1.RestartObservation{
				RestartCount:             2,
				CrashLoop:                true,
				AppliedRolloutGeneration: 1,
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	desired, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	got := desired.GetServices()[0].GetRestartObservation()
	if !got.GetCrashLoop() || got.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("desired observation = %+v", got)
	}
}
