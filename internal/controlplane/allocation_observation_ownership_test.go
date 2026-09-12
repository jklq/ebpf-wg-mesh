//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestAllocationObservationRejectsStaleSessionSequenceAndForeignOwner(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "owned", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocations, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocations) != 1 {
		t.Fatalf("allocations: %#v, %v", allocations, err)
	}
	allocation := allocations[0]
	report := observationReport(allocation, "test-session-node-1", 1, allocation.DesiredRolloutGeneration)
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); err != nil {
		t.Fatalf("initial observation: %v", err)
	}

	report.Services[0].Message = "must not overwrite"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); !errors.Is(err, deliverycore.ErrStaleObservation) {
		t.Fatalf("duplicate sequence: got %v", err)
	}
	obs, ok := fixtureLive(store).Observation(allocation.ID, allocation.DesiredRolloutGeneration)
	if !ok {
		t.Fatal("missing live observation")
	}
	if obs.Message == "must not overwrite" {
		t.Fatal("stale sequence overwrote the observation")
	}

	foreign := observationReport(allocation, "test-session-node-2", 1, allocation.DesiredRolloutGeneration)
	foreign.AgentId = "node-2"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-2", foreign); !errors.Is(err, deliverycore.ErrAllocationOwnership) {
		t.Fatalf("foreign allocation: got %v", err)
	}

	reconnected := agentHello("node-1")
	reconnected.SessionId = "replacement-session"
	if _, err := registerAgent(ctx, store, reconnected); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, "node-1", "test-session-node-1", false); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old-session heartbeat: got %v", err)
	}
	report.ObservationSequence = 2
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old session: got %v", err)
	}

	wrongAddress := observationReport(allocation, "replacement-session", 1, allocation.DesiredRolloutGeneration)
	wrongAddress.Services[0].AllocationIpv4 = "10.255.255.255"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", wrongAddress); !errors.Is(err, deliverycore.ErrAllocationOwnership) {
		t.Fatalf("address ownership: got %v", err)
	}
}

func TestOlderGenerationObservationCannotSatisfyCurrentAssignment(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, _ := store.catalog.listProjects(ctx, testUser("user-1"))
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "generation", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocations, _ := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	allocation := allocations[0]
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET desired_rollout_generation = $1 WHERE id = $2`, allocation.DesiredRolloutGeneration+1, allocation.ID); err != nil {
			return err
		}
		journal.RecordAssignment(ctx, allocation.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report := observationReport(allocation, "test-session-node-1", 1, allocation.DesiredRolloutGeneration)
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); err != nil {
		t.Fatalf("older observation: %v", err)
	}
	current, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Healthy || current[0].AppliedRolloutGeneration != 0 || deliverycore.AllocationReady(current[0]) {
		t.Fatalf("older generation satisfied current readiness: %+v", current[0])
	}
	if _, ok := fixtureLive(store).Observation(allocation.ID, allocation.DesiredRolloutGeneration); !ok {
		t.Fatal("older observation was not retained")
	}
}

func observationReport(allocation deliverycore.AllocationRecord, sessionID string, sequence uint64, generation int64) *agentv1.StatusReport {
	return &agentv1.StatusReport{
		AgentId: allocation.AgentID, SessionId: sessionID, ObservationSequence: sequence,
		Services: []*agentv1.ServiceCondition{{
			AllocationId: allocation.ID, ServiceId: allocation.ServiceID,
			DesiredSpecRevision: allocation.DesiredSpecRevision, AppliedSpecRevision: allocation.DesiredSpecRevision,
			DesiredRolloutGeneration: generation, AppliedRolloutGeneration: generation,
			AllocationIpv4: allocation.AllocationIPv4, AllocationIpv6: allocation.AllocationIPv6,
			Phase: "Healthy", Healthy: true,
		}},
	}
}
