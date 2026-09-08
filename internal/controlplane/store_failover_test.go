//go:build integration

package controlplane

import (
	"context"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"net/netip"
	"testing"
	"time"
)

func TestFailoverServicesFromAgentTargetsExpiredNode(t *testing.T) {
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
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-1", "node-2"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := createService(ctx, store, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	originalID := mustAllocationOnAgent(t, store, service.ID, "node-1").ID

	staleAt := time.Now().UTC().Add(-2 * deliverycore.AgentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-1'`, staleAt); err != nil {
		t.Fatal(err)
	}
	notified, environmentsChanged, err := newTestDelivery(store, nil, nil, nil).failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-deliverycore.AgentHealthyTTL))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 2 {
		t.Fatalf("expected old and new agents to be notified, got %v", notified)
	}
	if len(environmentsChanged) != 1 || environmentsChanged[0] != environmentID {
		t.Fatalf("changed environments = %v, want %s", environmentsChanged, environmentID)
	}
	got, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
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
	notified, environmentsChanged, err := newTestDelivery(store, nil, nil, nil).failoverServicesFromAgent(ctx, "node-1", time.Now().UTC().Add(-deliverycore.AgentHealthyTTL))
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
	lastSeen := now.Add(-2 * deliverycore.AgentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-stale'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := NewServiceFailoverReconciler(delivery, time.Second, deliverycore.AgentHealthyTTL)
	_, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := store.reads.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "node-stale" || agents[0].LifecycleState != deliverycore.AgentStateUnavailable {
		t.Fatalf("agents after reconcile = %+v, want node-stale unavailable", agents)
	}
}

func TestFailoverReconcilerTriggersStatelessServiceRollover(t *testing.T) {
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
		t.Fatalf("list projects: %v", err)
	}
	for _, agentID := range []string{"node-a", "node-b"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := createService(ctx, store, "user-1", environmentID, "failover-web", serviceSpec(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	original := mustAllocationOnAgent(t, store, service.ID, "node-a")
	originalID := original.ID
	if _, err := store.db.ExecContext(ctx,
		`UPDATE allocations
		    SET applied_spec_revision = desired_spec_revision,
		        applied_rollout_generation = desired_rollout_generation,
		        phase = 'Running', message = '', allocation_ipv6 = 'fd00:200::aa', healthy_ipv6_ports = $1, healthy = TRUE
		  WHERE service_id = $2`, []byte("[8080]"), service.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate a dead agent whose last heartbeat is already past the healthy TTL.
	now := time.Now().UTC()
	lastSeen := now.Add(-2 * deliverycore.AgentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-a'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.failoverNow = func() time.Time { return now }
	reconciler := NewServiceFailoverReconciler(delivery, time.Second, deliverycore.AgentHealthyTTL)
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile failover: %v", err)
	}
	if len(result.NotifyAgentIDs) == 0 {
		t.Fatalf("expected cluster notifications after rollover, got %v", result.NotifyAgentIDs)
	}

	replacement := requireNodeLossReplacement(t, store, service.ID, originalID, "node-a", "node-b")
	if replacement.Phase != "Pending" || replacement.Healthy ||
		len(replacement.HealthyIPv4Ports) != 0 || len(replacement.HealthyIPv6Ports) != 0 {
		t.Fatalf("replacement was not reset for the surviving agent: %+v", replacement)
	}
	if replacement.AppliedSpecRevision != 0 || replacement.AppliedRolloutGeneration != 0 {
		t.Fatalf("applied state was not cleared on the replacement: %+v", replacement)
	}
	// Addresses come from the owning node's prefixes, so failover must re-address
	// the workload on both families rather than carry the dead node's addresses over.
	survivor, err := store.reads.AgentByID(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []struct {
		name           string
		subnet         string
		replaced, lost string
	}{
		{"IPv4", survivor.WorkloadIPv4Subnet, replacement.AllocationIPv4, original.AllocationIPv4},
		{"IPv6", survivor.WorkloadIPv6Subnet, replacement.AllocationIPv6, original.AllocationIPv6},
	} {
		if family.replaced == "" {
			t.Fatalf("replacement has no %s address: %+v", family.name, replacement)
		}
		if family.replaced == family.lost {
			t.Fatalf("replacement reused the lost node's %s address %q", family.name, family.lost)
		}
		prefix, err := netip.ParsePrefix(family.subnet)
		if err != nil {
			t.Fatalf("parse node-b %s subnet %q: %v", family.name, family.subnet, err)
		}
		addr, err := netip.ParseAddr(family.replaced)
		if err != nil {
			t.Fatalf("parse replacement %s address %q: %v", family.name, family.replaced, err)
		}
		if !prefix.Contains(addr) {
			t.Fatalf("replacement %s address %q is outside node-b subnet %q", family.name, family.replaced, family.subnet)
		}
	}

	oldState, err := desiredStateForAgent(ctx, store, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	newState, err := desiredStateForAgent(ctx, store, "node-b")
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

func mustAllocationOnAgent(t *testing.T, store *persistence, serviceID, agentID string) deliverycore.AllocationRecord {
	t.Helper()
	for _, alloc := range mustListAllocations(t, store, context.Background(), serviceID) {
		if alloc.AgentID == agentID {
			return alloc
		}
	}
	t.Fatalf("no allocation for service %s on %s", serviceID, agentID)
	return deliverycore.AllocationRecord{}
}

func requireNodeLossReplacement(t *testing.T, store *persistence, serviceID, originalID, deadAgent, liveAgent string) deliverycore.AllocationRecord {
	t.Helper()
	allocs := mustListAllocations(t, store, context.Background(), serviceID)
	var lost, live []deliverycore.AllocationRecord
	for _, alloc := range allocs {
		switch {
		case alloc.ID == originalID:
			if alloc.AgentID != deadAgent || alloc.RolloutState != deliverycore.AllocationRolloutLost || alloc.Phase != "Unavailable" {
				t.Fatalf("original allocation was rewritten instead of marked lost: %+v", alloc)
			}
			lost = append(lost, alloc)
		case alloc.AgentID == liveAgent && alloc.RolloutState != deliverycore.AllocationRolloutLost:
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
