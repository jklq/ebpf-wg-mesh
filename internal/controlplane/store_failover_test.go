//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestFailoverServicesFromAgentTargetsExpiredNode(t *testing.T) {
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
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-1", "node-2"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := store.createService(ctx, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "node-1").ID

	staleAt := time.Now().UTC().Add(-2 * agentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-1'`, staleAt); err != nil {
		t.Fatal(err)
	}
	notified, environmentsChanged, err := store.failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-agentHealthyTTL))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 2 {
		t.Fatalf("expected old and new agents to be notified, got %v", notified)
	}
	if len(environmentsChanged) != 1 || environmentsChanged[0] != environmentID {
		t.Fatalf("changed environments = %v, want %s", environmentsChanged, environmentID)
	}
	got, err := store.serviceByID(ctx, "user-1", environmentID, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllocatedAgentID != "node-2" {
		t.Fatalf("service assigned to %s, want node-2", got.AllocatedAgentID)
	}
	requireNodeLossReplacement(t, store, service.ID, originalID, "node-1", "node-2")
}

func TestFailoverServicesFromAgentIgnoresFreshNode(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	notified, environmentsChanged, err := store.failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-agentHealthyTTL))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Fatalf("fresh node triggered notifications: %v", notified)
	}
	if len(environmentsChanged) != 0 {
		t.Fatalf("fresh node changed environments: %v", environmentsChanged)
	}
}

func TestFailoverReconcilerFindsPersistedStaleAgent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-stale")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	lastSeen := now.Add(-2 * agentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-stale'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	reconciler := NewServiceFailoverReconciler(store, nil, nil, nil, time.Second, agentHealthyTTL)
	reconciler.now = func() time.Time { return now }
	_, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := store.listAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "node-stale" || agents[0].LifecycleState != agentStateUnavailable {
		t.Fatalf("agents after reconcile = %+v, want node-stale unavailable", agents)
	}
}

func TestFailoverReconcilerTriggersStatelessServiceRollover(t *testing.T) {
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
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-a", "node-b"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := store.createService(ctx, "user-1", environmentID, "failover-web", serviceSpec(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "node-a").ID
	if _, err := store.db.ExecContext(ctx,
		`UPDATE allocations
		    SET applied_spec_revision = desired_spec_revision,
		        applied_rollout_generation = desired_rollout_generation,
		        phase = 'Running', message = '', allocation_ip = 'fd00:200::aa', healthy_ports = $1, healthy = TRUE
		  WHERE service_id = $2`, []byte("[8080]"), service.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate a dead agent whose last heartbeat is already past the healthy TTL.
	now := time.Now().UTC()
	lastSeen := now.Add(-2 * agentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-a'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	reconciler := NewServiceFailoverReconciler(store, nil, nil, nil, time.Second, agentHealthyTTL)
	reconciler.now = func() time.Time { return now }
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile failover: %v", err)
	}
	if len(result.NotifyAgentIDs) == 0 {
		t.Fatalf("expected cluster notifications after rollover, got %v", result.NotifyAgentIDs)
	}

	replacement := requireNodeLossReplacement(t, store, service.ID, originalID, "node-a", "node-b")
	if replacement.Phase != "Pending" || replacement.Healthy || replacement.AllocationIP != "" {
		t.Fatalf("replacement was not reset for the surviving agent: %+v", replacement)
	}
	if replacement.AppliedSpecRevision != 0 || replacement.AppliedRolloutGeneration != 0 {
		t.Fatalf("applied state was not cleared on the replacement: %+v", replacement)
	}

	oldState, err := store.desiredStateForAgent(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	newState, err := store.desiredStateForAgent(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(oldState.GetServices()) != 0 {
		t.Fatalf("failed agent still has desired services: %d", len(oldState.GetServices()))
	}
	if len(newState.GetServices()) != 1 || newState.GetServices()[0].GetServiceId() != service.ID {
		t.Fatalf("surviving agent desired services = %v, want service %s", newState.GetServices(), service.ID)
	}
	if newState.GetServices()[0].GetAllocationId() == originalID {
		t.Fatal("surviving agent still has the original allocation identity")
	}
}

func mustAllocationOnAgent(t *testing.T, store *Store, serviceID, agentID string) allocationRecord {
	t.Helper()
	for _, alloc := range mustListAllocations(t, store, context.Background(), serviceID) {
		if alloc.AgentID == agentID {
			return alloc
		}
	}
	t.Fatalf("no allocation for service %s on %s", serviceID, agentID)
	return allocationRecord{}
}

func requireNodeLossReplacement(t *testing.T, store *Store, serviceID, originalID, deadAgent, liveAgent string) allocationRecord {
	t.Helper()
	allocs := mustListAllocations(t, store, context.Background(), serviceID)
	var lost, live []allocationRecord
	for _, alloc := range allocs {
		switch {
		case alloc.ID == originalID:
			if alloc.AgentID != deadAgent || alloc.RolloutState != allocationRolloutLost || alloc.Phase != allocationPhaseUnavailable {
				t.Fatalf("original allocation was rewritten instead of marked lost: %+v", alloc)
			}
			lost = append(lost, alloc)
		case alloc.AgentID == liveAgent && alloc.RolloutState != allocationRolloutLost:
			live = append(live, alloc)
		}
	}
	if len(lost) != 1 {
		t.Fatalf("expected the original allocation to remain lost on %s, got %+v", deadAgent, allocs)
	}
	if len(live) != 1 || live[0].ID == originalID {
		t.Fatalf("expected a new allocation on %s, got %+v", liveAgent, allocs)
	}
	return live[0]
}
