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
		if _, err := store.upsertAgent(ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := store.createService(ctx, "user-1", environmentID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}

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
	var allocationAgent, phase string
	if err := store.db.QueryRowContext(ctx,
		`SELECT agent_id, phase FROM allocations WHERE service_id = $1`, service.ID,
	).Scan(&allocationAgent, &phase); err != nil {
		t.Fatal(err)
	}
	if allocationAgent != "node-2" || phase != "Pending" {
		t.Fatalf("allocation state = (%s, %s), want (node-2, Pending)", allocationAgent, phase)
	}
}

func TestFailoverServicesFromAgentIgnoresFreshNode(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
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

func TestServerRestoresPersistedAgentExpiryDeadline(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.upsertAgent(ctx, agentHello("node-stale")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	lastSeen := now.Add(-2 * agentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-stale'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	fired := make(chan string, 1)
	tracker := NewAgentExpiryTracker(ctx, agentHealthyTTL, func(_ context.Context, agentID string, _ time.Time) error {
		fired <- agentID
		return nil
	})
	t.Cleanup(tracker.Close)
	server := &Server{store: store, expiry: tracker}
	if err := server.restoreAgentExpiryDeadlines(ctx, now); err != nil {
		t.Fatal(err)
	}

	select {
	case agentID := <-fired:
		if agentID != "node-stale" {
			t.Fatalf("expired agent = %q, want node-stale", agentID)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not restore the persisted stale-agent deadline")
	}
}

func TestAgentExpiryTriggersStatelessServiceRollover(t *testing.T) {
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
		if _, err := store.upsertAgent(ctx, agentHello(agentID)); err != nil {
			t.Fatalf("upsert %s: %v", agentID, err)
		}
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	service, err := store.createService(ctx, "user-1", environmentID, "failover-web", serviceSpec(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE allocations
		    SET applied_spec_revision = desired_spec_revision,
		        applied_rollout_generation = desired_rollout_generation,
		        phase = 'Running', message = '', allocation_ip = 'fd00:200::aa', healthy_ports = $1, healthy = TRUE
		  WHERE service_id = $2`, []byte("[8080]"), service.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate a dead agent whose last heartbeat is already past the healthy TTL,
	// then drive the same AgentExpiryTracker path the control plane uses in production.
	now := time.Now().UTC()
	lastSeen := now.Add(-2 * agentHealthyTTL)
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = 'node-a'`, lastSeen); err != nil {
		t.Fatal(err)
	}

	type expiryResult struct {
		agentID  string
		notified []string
		err      error
	}
	done := make(chan expiryResult, 1)
	tracker := NewAgentExpiryTracker(ctx, agentHealthyTTL, func(ctx context.Context, agentID string, cutoff time.Time) error {
		notified, _, err := store.failoverServicesFromAgent(ctx, agentID, cutoff)
		done <- expiryResult{agentID: agentID, notified: notified, err: err}
		return err
	})
	t.Cleanup(tracker.Close)
	tracker.Restore("node-a", lastSeen, now)

	var fired expiryResult
	select {
	case fired = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent expiry did not fire service rollover")
	}
	if fired.err != nil {
		t.Fatalf("failoverServicesFromAgent: %v", fired.err)
	}
	if fired.agentID != "node-a" {
		t.Fatalf("expired agent = %q, want node-a", fired.agentID)
	}
	if len(fired.notified) == 0 {
		t.Fatalf("expected cluster notifications after rollover, got %v", fired.notified)
	}

	allocation, err := store.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AgentID != "node-b" {
		t.Fatalf("service assigned to %s, want node-b", allocation.AgentID)
	}
	if allocation.Phase != "Pending" || allocation.Healthy || allocation.AllocationIP != "" {
		t.Fatalf("allocation was not reset for the surviving agent: %+v", allocation)
	}
	if allocation.AppliedSpecRevision != 0 || allocation.AppliedRolloutGeneration != 0 {
		t.Fatalf("applied state was not cleared after rollover: %+v", allocation)
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
}
