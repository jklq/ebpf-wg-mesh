//go:build integration

package controlplane

import (
	"context"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/restartpolicy"
)

func TestRecordStatusReportPersistsCrashLoopAndWithdrawsIngress(t *testing.T) {
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "web.example.test", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}
	backends, err := store.listHealthyIngressBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 1 {
		t.Fatalf("expected healthy backend, got %#v", backends)
	}
	_, alloc, err := store.serviceStatus(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatal(err)
	}

	changed, envs, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		AgentId: "node-1",
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             alloc.ID,
			ServiceId:                service.ID,
			AppliedSpecRevision:      1,
			AppliedRolloutGeneration: 1,
			Phase:                    restartpolicy.PhaseCrashLoop,
			Message:                  "crash loop after non-zero exit",
			HealthyPorts:             []int32{8080},
			Healthy:                  false,
			Restart: &platformv1.RestartObservation{
				RestartCount:             1,
				CrashLoop:                true,
				AppliedRolloutGeneration: 1,
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
			},
		}},
	})
	if err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
	if !changed {
		t.Fatal("expected ingress change when crash-loop withdraws the backend")
	}
	if len(envs) != 1 {
		t.Fatalf("expected environment event, got %v", envs)
	}
	updated, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Phase != restartpolicy.PhaseCrashLoop || updated.Healthy || !updated.Restart.GetCrashLoop() {
		t.Fatalf("allocation not persisted as crash-loop: %+v", updated)
	}
	backends, err = store.listHealthyIngressBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 0 {
		t.Fatalf("crash-loop allocation still in ingress: %#v", backends)
	}
}

func TestRestartServiceIncrementsNonceAndRedeployClearsObservation(t *testing.T) {
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}
	current, ok, err := store.currentDeploymentForService(ctx, service.ID)
	if err != nil || !ok || current.State != deploymentStateActive {
		t.Fatalf("current deployment: %+v ok=%v err=%v", current, ok, err)
	}
	original, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.restartService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatalf("restartService: %v", err)
	}
	afterRestart := mustListAllocations(t, store, ctx, service.ID)
	if len(afterRestart) != 2 {
		t.Fatalf("rolling restart should overlap a replacement: %+v", afterRestart)
	}
	kept := allocationByID(t, store, service.ID, original.ID)
	if kept.ID != original.ID || kept.OperatorRestartNonce != 0 || kept.RolloutState != allocationRolloutServing || !kept.Healthy {
		t.Fatalf("restart mutated the serving allocation in place: %+v", kept)
	}
	var replacement allocationRecord
	for _, alloc := range afterRestart {
		if alloc.ID != original.ID {
			replacement = alloc
			break
		}
	}
	if replacement.ID == "" || replacement.RolloutState != allocationRolloutStarting {
		t.Fatalf("expected a starting replacement allocation, got %+v", afterRestart)
	}

	desired, err := store.desiredStateForAgent(ctx, "node-1")
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
	if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		Services: []*agentv1.ServiceCondition{{
			AllocationId: serving[0].ID, ServiceId: service.ID,
			Phase: restartpolicy.PhaseCrashLoop, Message: "crash loop",
			Restart: &platformv1.RestartObservation{CrashLoop: true, RestartCount: 3, AppliedRolloutGeneration: serving[0].DesiredRolloutGeneration},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.redeployService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
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
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	_, alloc, err := store.serviceStatus(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		Services: []*agentv1.ServiceCondition{{
			AllocationId: alloc.ID, ServiceId: service.ID,
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
	desired, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	got := desired.GetServices()[0].GetRestartObservation()
	if !got.GetCrashLoop() || got.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("desired observation = %+v", got)
	}
}
