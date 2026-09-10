//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestEnvironmentNetworkIdentitiesAreUniqueAndDeliveredToAgents(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{
		{ID: "user-1", Email: "user-1@example.com", Projects: []string{"one", "two"}},
	}}); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected two projects, got %d", len(projects))
	}
	environmentsOne, err := store.catalog.listEnvironments(ctx, "user-1", projects[0].ID)
	if err != nil || len(environmentsOne) != 1 {
		t.Fatalf("list first project environments: %#v: %v", environmentsOne, err)
	}
	environmentsTwo, err := store.catalog.listEnvironments(ctx, "user-1", projects[1].ID)
	if err != nil || len(environmentsTwo) != 1 {
		t.Fatalf("list second project environments: %#v: %v", environmentsTwo, err)
	}
	if environmentsOne[0].NetworkIdentity == 0 || environmentsTwo[0].NetworkIdentity == 0 || environmentsOne[0].NetworkIdentity == environmentsTwo[0].NetworkIdentity {
		t.Fatalf("expected distinct non-zero network identities: %#v %#v", environmentsOne, environmentsTwo)
	}
	staging, err := store.catalog.createEnvironment(ctx, "user-1", projects[0].ID, "Staging")
	if err != nil {
		t.Fatalf("create staging environment: %v", err)
	}
	if staging.NetworkIdentity == environmentsOne[0].NetworkIdentity {
		t.Fatalf("environments in one project shared a network identity: %#v %#v", environmentsOne[0], staging)
	}

	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatalf("upsertAgent: %v", err)
	}
	service, err := createScheduledService(ctx, store, "user-1", environmentsOne[0].ID, "web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	deployed, _, err := releaseEnvironmentForTest(ctx, store, "user-1", environmentsOne[0].ID)
	if err != nil || len(deployed) != 1 {
		t.Fatalf("releaseEnvironment: %#v: %v", deployed, err)
	}
	service = deployed[0]
	stagingService, err := createScheduledService(ctx, store, "user-1", staging.ID, "web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}))
	if err != nil {
		t.Fatalf("create staging service: %v", err)
	}
	stagingDeployed, _, err := releaseEnvironmentForTest(ctx, store, "user-1", staging.ID)
	if err != nil || len(stagingDeployed) != 1 {
		t.Fatalf("release staging environment: %#v: %v", stagingDeployed, err)
	}
	stagingService = stagingDeployed[0]
	for label, item := range map[string]deliverycore.ServiceRecord{"production": service, "staging": stagingService} {
		allocation, err := store.reads.allocationByServiceID(ctx, item.ID)
		if err != nil {
			t.Fatalf("load %s allocation: %v", label, err)
		}
		if err := store.markAllocationHealthyForTest(ctx, item.ID, allocation.AllocationIPv4, 8080); err != nil {
			t.Fatalf("mark %s healthy: %v", label, err)
		}
		obs, ok := store.live.Observation(allocation.ID, allocation.DesiredRolloutGeneration)
		if !ok {
			t.Fatalf("missing live observation for %s", label)
		}
		obs.HealthyIPv6Ports = []int32{8080}
		if _, err := store.live.RecordObservation(obs); err != nil {
			t.Fatalf("mark %s IPv6 healthy: %v", label, err)
		}
	}
	state, err := desiredStateForAgent(ctx, store, service.AllocatedAgentID)
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if len(state.GetServices()) != 2 {
		t.Fatalf("expected both environment services in desired state: %#v", state.GetServices())
	}
	servicesByID := make(map[string]*agentv1.DesiredService, len(state.GetServices()))
	for _, desired := range state.GetServices() {
		servicesByID[desired.GetServiceId()] = desired
	}
	productionDesired := servicesByID[service.ID]
	stagingDesired := servicesByID[stagingService.ID]
	if productionDesired.GetEnvironmentId() != environmentsOne[0].ID || productionDesired.GetNetworkIdentity() != environmentsOne[0].NetworkIdentity {
		t.Fatalf("production network identity was not delivered: %#v", productionDesired)
	}
	if stagingDesired.GetEnvironmentId() != staging.ID || stagingDesired.GetNetworkIdentity() != staging.NetworkIdentity {
		t.Fatalf("staging network identity was not delivered: %#v", stagingDesired)
	}
	if productionDesired.GetPrivateIpv6() == stagingDesired.GetPrivateIpv6() {
		t.Fatalf("environment workload addresses collided: %q", productionDesired.GetPrivateIpv6())
	}
	for label, desired := range map[string]*agentv1.DesiredService{
		"production": productionDesired,
		"staging":    stagingDesired,
	} {
		if desired.GetInternalHostname() != "web.mesh.internal" {
			t.Fatalf("%s service got internal hostname %q", label, desired.GetInternalHostname())
		}
		if len(desired.GetInternalHosts()) != 1 || desired.GetInternalHosts()[0].GetHostname() != "web.mesh.internal" {
			t.Fatalf("%s service received cross-environment internal hosts: %#v", label, desired.GetInternalHosts())
		}
		if desired.GetInternalHosts()[0].GetIpv4() != desired.GetPrivateIpv4() ||
			desired.GetInternalHosts()[0].GetIpv6() != desired.GetPrivateIpv6() {
			t.Fatalf("%s internal host points at %q/%q, want %q/%q", label,
				desired.GetInternalHosts()[0].GetIpv4(), desired.GetInternalHosts()[0].GetIpv6(),
				desired.GetPrivateIpv4(), desired.GetPrivateIpv6())
		}
	}
}

func TestAgentBootstrapTokensAreBoundDurableAndSingleUse(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	tokens := []config.AgentBootstrapToken{{AgentID: "node-1", Token: "one-time-secret"}}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("ensureAgentBootstrapTokens: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-2", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected agent binding rejection, got %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "one-time-secret")
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, errInvalidBootstrapToken) {
			t.Fatalf("unexpected concurrent consumption error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one token consumer, got %d", successes)
	}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("reseed bootstrap tokens: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected consumed token rejection after reseed, got %v", err)
	}
}

func TestRemovedAgentBootstrapTokenIsRevoked(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "old-secret"}}); err != nil {
		t.Fatalf("seed old token: %v", err)
	}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "new-secret"}}); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "old-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected removed token rejection, got %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "new-secret"); err != nil {
		t.Fatalf("consume replacement token: %v", err)
	}
}

func TestProjectCreationCreatesExactlyOneProductionEnvironment(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "user-1", "demo")
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	environments, err := store.catalog.listEnvironments(ctx, "user-1", project.ID)
	if err != nil {
		t.Fatalf("listEnvironments: %v", err)
	}
	if len(environments) != 1 || !environments[0].IsProduction || environments[0].NetworkIdentity == 0 {
		t.Fatalf("unexpected production environments: %#v", environments)
	}
}
