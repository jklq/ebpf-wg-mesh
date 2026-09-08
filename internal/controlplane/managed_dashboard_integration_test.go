//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"testing"
	"time"

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

	project, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	spec := directImageServiceSpec("example.test/dashboard:latest", &platformv1.ServiceRuntime{
		CpuMillis:       10_000,
		MemoryMebibytes: 10_000,
	})
	service, _, err := newTestDelivery(store, nil, nil, nil).EnsureManagedService(ctx, project.ID, "dashboard", spec, trusted.AgentId)
	if err != nil {
		t.Fatalf("ensureManagedService: %v", err)
	}
	if service.AllocatedAgentID != trusted.AgentId {
		t.Fatalf("dashboard allocated to %q, want trusted agent %q", service.AllocatedAgentID, trusted.AgentId)
	}

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{{
		ID: "user-1", Email: "user@example.test", Projects: []string{"demo"},
	}}}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v (%d projects)", err, len(projects))
	}
	userService, err := createScheduledService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "app", directImageServiceSpec("example.test/app:latest", nil))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	if userService.AllocatedAgentID == trusted.AgentId {
		t.Fatal("user workload was placed on reserved dashboard agent")
	}
}

func TestManagedDashboardSameAgentSyncPreservesServingAllocationState(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	trusted := agentHello("trusted-dashboard-node")
	if _, err := upsertTestAgent(t, store, ctx, trusted); err != nil {
		t.Fatal(err)
	}
	store.reserveAgents(trusted.AgentId)
	project, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	delivery := newTestDelivery(store, nil, nil, nil)
	service, _, err := delivery.EnsureManagedService(ctx, project.ID, "dashboard", directImageServiceSpec("example.test/dashboard:1", nil), trusted.AgentId)
	if err != nil {
		t.Fatal(err)
	}
	serving := completeManagedAllocation(t, store, ctx, service.ID)

	updated, _, err := delivery.EnsureManagedService(ctx, project.ID, "dashboard", directImageServiceSpec("example.test/dashboard:2", nil), trusted.AgentId)
	if err != nil {
		t.Fatalf("ensureManagedService(update): %v", err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 1 {
		t.Fatalf("allocations = %+v", allocations)
	}
	got := allocations[0]
	if got.ID != serving.ID || got.AgentID != trusted.AgentId {
		t.Fatalf("same-agent sync replaced or moved allocation: %+v", got)
	}
	if got.Phase != "Healthy" || !got.Healthy || got.RolloutState != deliverycore.AllocationRolloutServing {
		t.Fatalf("same-agent sync punched serving state: %+v", got)
	}
	if got.DesiredSpecRevision != updated.SpecRevision || got.DesiredRolloutGeneration != updated.RolloutGeneration {
		t.Fatalf("desired generation was not updated: allocation=%+v service=%+v", got, updated)
	}
}

func TestManagedDashboardTrustedAgentChangeStartsRollingReplacement(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	oldTrusted := agentHello("trusted-dashboard-old")
	newTrusted := agentHello("trusted-dashboard-new")
	if _, err := upsertTestAgent(t, store, ctx, oldTrusted); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, newTrusted); err != nil {
		t.Fatal(err)
	}
	store.reserveAgents(oldTrusted.AgentId, newTrusted.AgentId)
	project, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	spec := directImageServiceSpec("example.test/dashboard:1", nil)
	delivery := newTestDelivery(store, nil, nil, nil)
	service, _, err := delivery.EnsureManagedService(ctx, project.ID, "dashboard", spec, oldTrusted.AgentId)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := completeManagedAllocation(t, store, ctx, service.ID)

	updated, _, err := delivery.EnsureManagedService(ctx, project.ID, "dashboard", spec, newTrusted.AgentId)
	if err != nil {
		t.Fatalf("ensureManagedService(move): %v", err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 2 {
		t.Fatalf("expected overlapping replacement, got %+v", allocations)
	}
	var oldAllocation, replacement deliverycore.AllocationRecord
	for _, allocation := range allocations {
		switch allocation.AgentID {
		case oldTrusted.AgentId:
			oldAllocation = allocation
		case newTrusted.AgentId:
			replacement = allocation
		}
	}
	if oldAllocation.ID != predecessor.ID || oldAllocation.RolloutState != deliverycore.AllocationRolloutServing || oldAllocation.Phase != "Healthy" {
		t.Fatalf("predecessor was teleported or withdrawn before replacement readiness: %+v", oldAllocation)
	}
	if replacement.ID == "" || replacement.ID == predecessor.ID || replacement.RolloutState != deliverycore.AllocationRolloutStarting || replacement.Phase != "Pending" {
		t.Fatalf("trusted-agent replacement was not started honestly: %+v", replacement)
	}
	if replacement.DesiredSpecRevision != updated.SpecRevision || replacement.DesiredRolloutGeneration != updated.RolloutGeneration {
		t.Fatalf("replacement generation = %+v, service = %+v", replacement, updated)
	}
	if _, _, err := delivery.EnsureManagedService(ctx, project.ID, "dashboard", spec, newTrusted.AgentId); err != nil {
		t.Fatalf("idempotent ensureManagedService(move): %v", err)
	}
	if got := mustListAllocations(t, store, ctx, service.ID); len(got) != 2 {
		t.Fatalf("idempotent sync created another replacement: %+v", got)
	}
}

func completeManagedAllocation(t *testing.T, store *persistence, ctx context.Context, serviceID string) deliverycore.AllocationRecord {
	t.Helper()
	allocation, err := store.reads.allocationByServiceID(ctx, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE allocations
		    SET applied_spec_revision = desired_spec_revision,
		        applied_rollout_generation = desired_rollout_generation,
		        allocation_ipv6 = 'fd00:200::10', phase = 'Healthy', healthy = TRUE,
		        rollout_state = $1, updated_at = $2
		  WHERE id = $3`,
		deliverycore.AllocationRolloutServing, time.Now().UTC(), allocation.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	allocation, err = store.reads.allocationByServiceID(ctx, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}
