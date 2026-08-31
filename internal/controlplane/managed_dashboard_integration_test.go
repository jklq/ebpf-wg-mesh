//go:build integration

package controlplane

import (
	"context"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestManagedDashboardUsesReservedTrustedAgentWithoutReportedCapacity(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	trusted := agentHello("trusted-dashboard-node")
	trusted.CpuMillisCapacity = 1
	trusted.MemoryMebibytesCapacity = 1
	if _, err := upsertTestAgent(t, store, ctx, trusted); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("pool-node")); err != nil {
		t.Fatal(err)
	}
	store.reserveAgents(trusted.AgentId)

	project, err := store.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	spec := directImageServiceSpec("example.test/dashboard:latest", &platformv1.ServiceRuntime{
		CpuMillis:       10_000,
		MemoryMebibytes: 10_000,
	})
	service, _, err := store.ensureManagedService(ctx, project.ID, "dashboard", spec, trusted.AgentId)
	if err != nil {
		t.Fatalf("ensureManagedService: %v", err)
	}
	if service.AllocatedAgentID != trusted.AgentId {
		t.Fatalf("dashboard allocated to %q, want trusted agent %q", service.AllocatedAgentID, trusted.AgentId)
	}

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{{
		ID: "user-1", Email: "user@example.test", Projects: []string{"demo"},
	}}}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v (%d projects)", err, len(projects))
	}
	userService, err := store.createScheduledService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "app", directImageServiceSpec("example.test/app:latest", nil))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	if userService.AllocatedAgentID == trusted.AgentId {
		t.Fatal("user workload was placed on reserved dashboard agent")
	}
}
