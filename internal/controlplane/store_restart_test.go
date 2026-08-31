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
	if _, _, err := store.applyDeploymentAction(ctx, "user-1", service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, "restart-1", ""); err != nil {
		t.Fatalf("applyDeploymentAction restart: %v", err)
	}
	afterRestart, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.OperatorRestartNonce != 1 || afterRestart.Phase != "Pending" {
		t.Fatalf("operator restart = %+v", afterRestart)
	}

	desired, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.GetServices()) != 1 || desired.GetServices()[0].GetOperatorRestartNonce() != 1 {
		t.Fatalf("desired state missing operator nonce: %#v", desired.GetServices())
	}

	afterRestart, err = store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.recordStatusReport(ctx, "node-1", &agentv1.StatusReport{
		Services: []*agentv1.ServiceCondition{{
			AllocationId: afterRestart.ID, ServiceId: service.ID,
			Phase: restartpolicy.PhaseCrashLoop, Message: "crash loop",
			Restart: &platformv1.RestartObservation{CrashLoop: true, RestartCount: 3, AppliedRolloutGeneration: afterRestart.DesiredRolloutGeneration},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.redeployService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	afterRollout, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
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
