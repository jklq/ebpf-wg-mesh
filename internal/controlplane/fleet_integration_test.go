//go:build integration

package controlplane

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestUnenrolledAgentCannotSelfRegister(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := registerAgent(ctx, store, agentHello("ghost")); !errors.Is(err, deliverycore.ErrAgentNotEnrolled) {
		t.Fatalf("upsertAgent(ghost): got %v, want %v", err, deliverycore.ErrAgentNotEnrolled)
	}
}

func TestFleetDrainMovesStatelessReplicasAndBlocksWithoutCapacity(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	seedFleetOperator(t, store, ctx)

	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 1)
	completeServingAllocations(t, store, service.ID)
	original := mustListAllocations(t, store, ctx, service.ID)
	if agents := allocationAgentIDs(original); agents["node-a"] != 1 || len(original) != 1 {
		t.Fatalf("expected replica on node-a, got %v", agents)
	}
	originalID := original[0].ID

	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-b", deliverycore.AgentStateCordoned); err != nil {
		t.Fatalf("cordon node-b: %v", err)
	}
	draining, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-a", deliverycore.AgentStateDraining)
	if err != nil {
		t.Fatalf("drain node-a without alternate capacity: %v", err)
	}
	if !strings.Contains(draining.MaintenanceMessage, "drain paused") {
		t.Fatalf("expected drain interruption message, got %q", draining.MaintenanceMessage)
	}
	if agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID)); agents["node-a"] != 1 || agents["node-b"] != 0 {
		t.Fatalf("drain without capacity moved the replica: %v", agents)
	}

	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-b", deliverycore.AgentStateActive); err != nil {
		t.Fatalf("return node-b: %v", err)
	}
	if _, err := newTestDelivery(store, nil, nil, nil).reconcileDrainingAgent(ctx, "node-a"); err != nil {
		t.Fatalf("retry drain: %v", err)
	}
	afterStart := mustListAllocations(t, store, ctx, service.ID)
	agents := allocationAgentIDs(afterStart)
	if agents["node-a"] != 1 || agents["node-b"] != 1 {
		t.Fatalf("expected overlapping rolling replacement on node-b, got %v", agents)
	}
	var replacement deliverycore.AllocationRecord
	keptOriginal := false
	for _, alloc := range afterStart {
		if alloc.ID == originalID {
			keptOriginal = alloc.AgentID == "node-a"
			continue
		}
		if alloc.AgentID == "node-b" {
			replacement = alloc
		}
	}
	if !keptOriginal {
		t.Fatalf("drain rewrote the original allocation instead of creating a replacement: %+v", afterStart)
	}
	if replacement.ID == "" || replacement.ID == originalID || replacement.RolloutState != deliverycore.AllocationRolloutStarting {
		t.Fatalf("expected a starting replacement allocation on node-b, got %+v", replacement)
	}
	inProgress, err := store.reads.AgentByID(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inProgress.MaintenanceMessage, "drain in progress") {
		t.Fatalf("expected in-progress drain, got %q", inProgress.MaintenanceMessage)
	}

	markRolloutAllocationReady(t, store, replacement)
	probe := &rolloutIngressProbe{store: store}
	reconciler := newRolloutReconciler(newTestDelivery(store, nil, probe, nil), time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("promote replacement: %v", err)
	}
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("finish drain: %v", err)
	}
	if agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID)); agents["node-b"] != 1 || agents["node-a"] != 0 {
		t.Fatalf("expected drain to finish on node-b, got %v", agents)
	}
	finished, err := store.reads.AgentByID(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(finished.MaintenanceMessage, "drain complete") {
		t.Fatalf("expected completed drain, got %q", finished.MaintenanceMessage)
	}
}

func completeServingAllocations(t *testing.T, store *persistence, serviceID string) {
	t.Helper()
	ctx := context.Background()
	delivery := newTestDelivery(store, nil, nil, nil)
	for step := 0; step < 8; step++ {
		for _, alloc := range mustListAllocations(t, store, ctx, serviceID) {
			if alloc.RolloutState == deliverycore.AllocationRolloutStarting {
				markRolloutAllocationReady(t, store, alloc)
			}
		}
		if err := delivery.ReconcileRollouts(ctx); err != nil {
			t.Fatalf("advance rollout: %v", err)
		}

		markAllDrainingComplete(t, store, serviceID)
		if err := delivery.ReconcileRollouts(ctx); err != nil {
			t.Fatalf("remove drained allocations: %v", err)
		}
		remaining := mustListAllocations(t, store, ctx, serviceID)
		if len(remaining) == 0 {
			continue
		}
		allServing := true
		for _, alloc := range remaining {
			if alloc.RolloutState != deliverycore.AllocationRolloutServing {
				allServing = false
				break
			}
		}
		if allServing {
			return
		}
	}
	t.Fatalf("could not complete a serving rollout: %+v", mustListAllocations(t, store, ctx, serviceID))
}

func TestFleetNodeReturnPlacesPendingReplicas(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	hello := agentHello("node-a")
	hello.CpuMillisCapacity = 150
	hello.MemoryMebibytesCapacity = 128
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}

	spec := replicaSpec(100, 64)
	spec.DesiredReplicaCount = replicaCountPtr(2)
	service, err := createService(ctx, store, "user-1", envID, "web", spec, "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	placed := mustListAllocations(t, store, ctx, service.ID)
	if len(placed) != 1 {
		t.Fatalf("expected one placed replica before node return, got %d", len(placed))
	}
	pending, err := store.reads.ServiceByID(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pending.PlacementMessage, "1 of 2 replicas placed") {
		t.Fatalf("expected pending placement message, got %q", pending.PlacementMessage)
	}

	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-b")); err != nil {
		t.Fatal(err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileFleetCapacity(ctx); err != nil {
		t.Fatalf("reconcileFleetCapacity: %v", err)
	}
	after := mustListAllocations(t, store, ctx, service.ID)
	if len(after) != 2 {
		t.Fatalf("expected node return to place the pending replica, got %d", len(after))
	}
	cleared, err := store.reads.ServiceByID(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PlacementMessage != "" {
		t.Fatalf("expected placement message to clear after node return, got %q", cleared.PlacementMessage)
	}
}

func TestFleetReplicaSpreadAndRegionPendingReason(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b", "node-c"})
	seedFleetOperator(t, store, ctx)
	for _, req := range []*platformv1.UpdateAgentRequest{
		{AgentId: "node-a", Name: "node-a", Region: "us-east", FailureDomain: "zone-1"},
		{AgentId: "node-b", Name: "node-b", Region: "us-east", FailureDomain: "zone-1"},
		{AgentId: "node-c", Name: "node-c", Region: "us-east", FailureDomain: "zone-2"},
	} {
		if _, err := testDelivery(store).UpdateFleetAgent(ctx, "ops", req); err != nil {
			t.Fatalf("updateFleetAgent(%s): %v", req.AgentId, err)
		}
	}

	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 2 {
		t.Fatalf("expected 2 allocations, got %d", len(allocations))
	}
	agents := allocationAgentIDs(allocations)
	if agents["node-c"] != 1 || (agents["node-a"]+agents["node-b"] != 1) {
		t.Fatalf("expected replicas spread across zone-1 and zone-2, got %v", agents)
	}

	regionSpec := replicaSpec(100, 64)
	regionSpec.PlacementRegion = "eu-west"
	regionService, err := createService(ctx, store, "user-1", envID, "eu-web", regionSpec, "node-a")
	if err != nil {
		t.Fatalf("createService(region): %v", err)
	}
	pending, err := store.reads.ServiceByID(ctx, testUser("user-1"), regionService.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pending.PlacementMessage, `region "eu-west"`) {
		t.Fatalf("expected region pending explanation, got %q", pending.PlacementMessage)
	}
}

func TestFleetRetirementRevokesCredentialsAndMeshIdentity(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	seedFleetOperator(t, store, ctx)
	hello := agentHello("node-retire")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := store.fleet.RecordAgentCertificate(ctx, "node-retire", "abcd"); err != nil {
		t.Fatalf("RecordAgentCertificate: %v", err)
	}
	path := t.TempDir() + "/revoked.txt"
	revocations, err := NewCertificateRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	service := newOpsService(nil, store.fleet, newDelivery(store, nil, nil, nil, nil), nil, revocations)
	opsContext := contextWithDelegatedUser("ops", "ops@example.com")
	if _, err := service.SetAgentLifecycle(opsContext, &platformv1.SetAgentLifecycleRequest{
		AgentId:        "node-retire",
		LifecycleState: platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED,
	}); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	retired, err := service.SetAgentLifecycle(opsContext, &platformv1.SetAgentLifecycleRequest{
		AgentId:        "node-retire",
		LifecycleState: platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED,
	})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	rec, err := store.reads.AgentByID(ctx, "node-retire")
	if err != nil {
		t.Fatalf("agentByID: %v", err)
	}
	if rec.LifecycleState != deliverycore.AgentStateRetired || !rec.CredentialRevokedAt.Valid {
		t.Fatalf("expected retired revoked agent, got %+v", rec)
	}
	if retired.GetLifecycleState() != platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED {
		t.Fatalf("OpsService returned lifecycle %s", retired.GetLifecycleState())
	}
	if rec.WireGuardPublicKey != "" || rec.WireGuardEndpoint != "" || rec.WorkloadIPv6Subnet != "" {
		t.Fatalf("expected mesh identity to be cleared, got %+v", rec)
	}
	if err := store.fleet.AuthorizeAgentCredential(ctx, "node-retire"); !errors.Is(err, deliverycore.ErrAgentCredentialRevoked) {
		t.Fatalf("AuthorizeAgentCredential: got %v", err)
	}
	if _, err := registerAgent(ctx, store, hello); !errors.Is(err, deliverycore.ErrAgentCredentialRevoked) {
		t.Fatalf("upsert after retire: got %v", err)
	}
	serials, err := store.fleet.listAgentCertificateSerials(ctx, "node-retire")
	if err != nil || len(serials) != 1 || serials[0] != "abcd" {
		t.Fatalf("certificate serials = %v, err=%v", serials, err)
	}

	if err := revocations.Check(&x509.Certificate{SerialNumber: new(big.Int).SetInt64(0xabcd)}); !errors.Is(err, identity.ErrClientCertificateRevoked) {
		t.Fatalf("production CRL was not updated by OpsService: %v", err)
	}
}

func TestFleetViewReportsHeadroomAndVersionSkew(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	seedFleetOperator(t, store, ctx)
	left := agentHello("node-a")
	left.SoftwareVersion = "1.0.0"
	right := agentHello("node-b")
	right.SoftwareVersion = "1.0.1"
	right.AdvertiseAddr = "fd00:30::11"
	if _, err := upsertTestAgent(t, store, ctx, left); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, right); err != nil {
		t.Fatal(err)
	}
	if _, err := testDelivery(store).UpdateFleetAgent(ctx, "ops", &platformv1.UpdateAgentRequest{
		AgentId: "node-a", Name: "node-a", Region: "us-east", FailureDomain: "zone-1", ReservedCpuMillis: 500,
	}); err != nil {
		t.Fatalf("reserve CPU: %v", err)
	}
	fleet, err := store.fleet.fleetView(ctx, testUser("ops"))
	if err != nil {
		t.Fatalf("fleetView: %v", err)
	}
	if fleet.GetCapacity().GetSchedulableNodeCount() != 2 {
		t.Fatalf("schedulable nodes = %d", fleet.GetCapacity().GetSchedulableNodeCount())
	}
	if fleet.GetVersionWarning() == "" {
		t.Fatal("expected version skew warning")
	}
	var reserved *platformv1.Agent
	for _, agent := range fleet.GetAgents() {
		if agent.GetId() == "node-a" {
			reserved = agent
		}
	}
	if reserved == nil || reserved.GetSchedulableCpuMillis() != 1500 {
		t.Fatalf("expected reserved CPU to reduce schedulable capacity, got %+v", reserved)
	}
	if reserved.GetWireguardEndpoint() != left.GetWireguardEndpoint() {
		t.Fatalf("fleet WireGuard endpoint = %q, want %q", reserved.GetWireguardEndpoint(), left.GetWireguardEndpoint())
	}
}

func TestCordonedNodesAreExcludedFromNewPlacement(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	seedFleetOperator(t, store, ctx)
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-b", deliverycore.AgentStateCordoned); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", envID, "web", replicaSpec(100, 64), "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	mustQueueAndDeployReplicas(t, store, ctx, envID, service.ID, 2)
	agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID))
	if agents["node-a"] != 2 || agents["node-b"] != 0 {
		t.Fatalf("cordoned node received a new replica: %v", agents)
	}
}

func TestDisconnectedNodesAreExcludedFromNewPlacement(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	if err := newTestDelivery(store, nil, nil, nil).EndAgentSession(ctx, "node-a", "test-session-node-a"); err != nil {
		t.Fatalf("end node-a session: %v", err)
	}

	agentID, err := chooseAgentForService(ctx, store, envID, replicaSpec(100, 64))
	if err != nil {
		t.Fatalf("chooseAgentForService: %v", err)
	}
	if agentID != "node-b" {
		t.Fatalf("new placement selected disconnected agent %q, want node-b", agentID)
	}
}

func TestAgentReconnectPreservesOperatorAdministration(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	seedReplicaFixture(t, store, ctx, []string{"node-a"})
	seedFleetOperator(t, store, ctx)
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-a", deliverycore.AgentStateCordoned); err != nil {
		t.Fatalf("cordon node-a: %v", err)
	}

	hello := agentHello("node-a")
	hello.SessionId = "replacement-session"
	if _, err := registerAgent(ctx, store, hello); err != nil {
		t.Fatalf("reconnect node-a: %v", err)
	}
	agent, err := store.reads.AgentByID(ctx, "node-a")
	if err != nil {
		t.Fatalf("read node-a: %v", err)
	}
	if agent.LifecycleState != deliverycore.AgentStateCordoned {
		t.Fatalf("reconnect changed operator administration to %q", agent.LifecycleState)
	}
}

func seedFleetOperator(t *testing.T, store *persistence, ctx context.Context) {
	t.Helper()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "ops", Email: "ops@example.com", Operator: true}},
	}); err != nil {
		t.Fatalf("EnsureBootstrap(operator): %v", err)
	}
}

func TestStatefulDrainRemainsFenced(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	seedFleetOperator(t, store, ctx)
	if _, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "data", 64<<20); err != nil {
		t.Fatalf("createScheduledVolume: %v", err)
	}
	spec := replicaSpec(100, 64)
	spec.Runtime.Volume = &platformv1.ServiceVolumeMount{VolumeName: "data"}
	service, err := createService(ctx, store, "user-1", envID, "disk", spec, "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, testUser("ops"), "node-a", deliverycore.AgentStateDraining); err != nil {
		t.Fatalf("drain: %v", err)
	}
	agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID))
	if agents["node-a"] != 1 {
		t.Fatalf("stateful allocation was moved before Stage 7: %v", agents)
	}
	rec, err := store.reads.AgentByID(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.MaintenanceMessage, "Stage 7") {
		t.Fatalf("expected fenced stateful drain message, got %q", rec.MaintenanceMessage)
	}
}

func TestRegisterAgentRejectsMissingWireGuardEndpoint(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("missing-endpoint")
	if err := enrollTestAgent(ctx, store, hello); err != nil {
		t.Fatal(err)
	}
	hello.WireguardEndpoint = ""
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err == nil || !strings.Contains(err.Error(), "wireguard_endpoint") {
		t.Fatalf("missing endpoint error = %v", err)
	}
}

func TestIPv4NodePrefixAllocationRejectsExhaustionAndOverlapTransactionally(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *persistence)
	}{
		{
			name: "exhaustion",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				for i, want := range []string{"10.42.0.0/30", "10.42.0.4/30"} {
					hello := testAgentHello(i + 1)
					if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
						t.Fatalf("upsertAgent(%s): %v", hello.GetAgentId(), err)
					}
					agent, err := store.reads.AgentByID(ctx, hello.GetAgentId())
					if err != nil {
						t.Fatal(err)
					}
					if agent.WorkloadIPv4Subnet != want {
						t.Fatalf("agent %s prefix = %q, want %q", hello.GetAgentId(), agent.WorkloadIPv4Subnet, want)
					}
				}

				hello := testAgentHello(3)
				if err := enrollTestAgent(ctx, store, hello); err != nil {
					t.Fatal(err)
				}
				if _, err := registerAgent(ctx, store, hello); err == nil || !strings.Contains(err.Error(), "exhausted") {
					t.Fatalf("expected pool exhaustion, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 2)
			},
		},
		{
			name: "overlap",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := enrollTestAgent(ctx, store, testAgentHello(2)); err != nil {
					t.Fatal(err)
				}
				if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET workload_ipv4_subnet = '10.42.0.1/30' WHERE id = 'node-2'`); err != nil {
						return err
					}
					journal.AgentRow("node-2").Capture(ctx)
					return nil
				}); err != nil {
					t.Fatal(err)
				}

				hello := testAgentHello(3)
				if err := enrollTestAgent(ctx, store, hello); err != nil {
					t.Fatal(err)
				}
				if _, err := registerAgent(ctx, store, hello); err == nil || !strings.Contains(err.Error(), "overlap") {
					t.Fatalf("expected overlap rejection, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 1)
			},
		},
		{
			name: "workload address exhaustion",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
					Users: []config.BootstrapUser{{ID: "user-1", Projects: []string{"demo"}}},
				}); err != nil {
					t.Fatal(err)
				}
				projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
				if err != nil || len(projects) != 1 {
					t.Fatalf("listProjects: projects=%d err=%v", len(projects), err)
				}
				environmentID := productionEnvironmentID(t, store, projects[0].ID)
				if _, err := createService(ctx, store, "user-1", environmentID, "first", serviceSpec(), "node-1"); err != nil {
					t.Fatal(err)
				}
				if _, err := createService(ctx, store, "user-1", environmentID, "second", serviceSpec(), "node-1"); err == nil || !strings.Contains(err.Error(), "exhausted") {
					t.Fatalf("expected address exhaustion, got %v", err)
				}
				var services, allocations int
				if err := store.db.QueryRow(`SELECT count(*) FROM services`).Scan(&services); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow(`SELECT count(*) FROM allocations`).Scan(&allocations); err != nil {
					t.Fatal(err)
				}
				if services != 1 || allocations != 1 {
					t.Fatalf("exhausted allocation partially committed: services=%d allocations=%d", services, allocations)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meshCfg := testMeshConfig()
			meshCfg.WorkloadIPv4PoolCIDR = "10.42.0.0/29"
			meshCfg.WorkloadIPv4NodePrefixBits = 30
			store, err := openPersistence(config.DatabaseConfig{URL: createTestDatabase(t)}, meshCfg)
			if err != nil {
				t.Fatal(err)
			}
			delivery := newDelivery(store, nil, nil, nil, nil)
			if err := delivery.BecomeLive(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				delivery.ResignLive()
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			tc.run(t, store)
		})
	}
}

func assertIPv4AllocatorUnchanged(t *testing.T, store *persistence, unassignedAgent string, wantOrdinal int64) {
	t.Helper()
	var ordinal int64
	if err := store.db.QueryRow(`SELECT next_ordinal FROM workload_ipv4_prefix_allocator WHERE id = TRUE`).Scan(&ordinal); err != nil {
		t.Fatal(err)
	}
	if ordinal != wantOrdinal {
		t.Fatalf("allocator ordinal = %d, want %d after rejected transaction", ordinal, wantOrdinal)
	}
	var prefix string
	if err := store.db.QueryRow(`SELECT workload_ipv4_subnet FROM agents WHERE id = $1`, unassignedAgent).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if prefix != "" {
		t.Fatalf("rejected agent received prefix %q", prefix)
	}
}

func TestAssignedNodeConfigSharedEnvironments(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "mesh")
	if err != nil {
		t.Fatal(err)
	}
	envA := productionEnvironmentID(t, store, project.ID)
	environmentB, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := upsertTestAgent(t, store, ctx, testAgentHello(i)); err != nil {
			t.Fatal(err)
		}
	}
	add := func(env, name, agent string) deliverycore.ServiceRecord {
		t.Helper()
		service, err := createService(ctx, store, "owner", env, name, serviceSpec(), agent)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	add(envA, "a", "node-1")
	add(environmentB.ID, "b", "node-2")
	add(environmentB.ID, "c", "node-3")
	check := func(agentID string, peers []string, identities int) {
		t.Helper()
		cfg, err := testDelivery(store).assignedNodeConfigForAgent(ctx, agentID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, peer := range cfg.GetPeers() {
			got = append(got, peer.GetAgentId())
			host, err := store.reads.AgentByID(ctx, peer.GetAgentId())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(peer.GetAllowedIps(), []string{host.WorkloadIPv4Subnet, host.WorkloadIPv6Subnet}) {
				t.Fatalf("peer prefixes = %v", peer.GetAllowedIps())
			}
		}
		if !slices.Equal(got, peers) || len(cfg.GetWorkloadIdentities()) != identities {
			t.Fatalf("%s peers=%v identities=%d, want %v/%d", agentID, got, len(cfg.GetWorkloadIdentities()), peers, identities)
		}
		if cfg.GetWorkloadIpv4Pool() == "" || cfg.GetWorkloadIpv6Pool() == "" {
			t.Fatal("missing fail-closed workload pools")
		}
	}
	check("node-1", nil, 1)
	check("node-2", []string{"node-3"}, 2)
	beforeA := mustDesiredRevision(t, store, ctx, "node-1")
	add(environmentB.ID, "b-growth", "node-3")
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != beforeA {
		t.Fatalf("disjoint environment growth bumped node-1: %d -> %d", beforeA, got)
	}
	check("node-1", nil, 1)
	shared := add(envA, "shared", "node-2")
	check("node-1", []string{"node-2"}, 2)
	check("node-2", []string{"node-1", "node-3"}, 5)
	check("node-3", []string{"node-2"}, 3)

	before := make(map[string]int64)
	for _, id := range []string{"node-1", "node-2", "node-3"} {
		before[id] = mustDesiredRevision(t, store, ctx, id)
	}
	hello := testAgentHello(1)
	hello.WireguardEndpoint = "192.0.2.99:51820"
	if _, err := registerAgent(ctx, store, hello); err != nil {
		t.Fatal(err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != before["node-2"]+1 {
		t.Fatal("endpoint change did not update shared peer")
	}
	if got := mustDesiredRevision(t, store, ctx, "node-3"); got != before["node-3"] {
		t.Fatal("endpoint change updated disjoint peer")
	}
	before["node-1"] = mustDesiredRevision(t, store, ctx, "node-1")
	before["node-2"] = mustDesiredRevision(t, store, ctx, "node-2")
	if err := deleteService(ctx, store, "owner", shared.ID); err != nil {
		t.Fatal(err)
	}
	check("node-1", nil, 1)
	check("node-2", []string{"node-3"}, 3)
	for _, id := range []string{"node-1", "node-2"} {
		if got := mustDesiredRevision(t, store, ctx, id); got != before[id]+1 {
			t.Fatalf("last shared allocation removal did not update %s", id)
		}
	}
	if got := mustDesiredRevision(t, store, ctx, "node-3"); got != before["node-3"] {
		t.Fatal("environment A removal updated environment B")
	}
}

func TestDesiredStateDistributesCrossNodeWorkloadIdentities(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	for i, host := range []string{"fd00:30::10", "fd00:30::11"} {
		hello := testAgentHello(i + 1)
		hello.AdvertiseAddr = host
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatalf("upsertAgent: %v", err)
		}
	}
	services := make([]deliverycore.ServiceRecord, 0, 2)
	for i, agentID := range []string{"node-1", "node-2"} {
		service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), fmt.Sprintf("web-%d", i+1), directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
			Ports: runtimePortsFromInts([]int32{8080}),
		}), agentID)
		if err != nil {
			t.Fatalf("createService(%s): %v", agentID, err)
		}
		services = append(services, service)
	}

	for _, agentID := range []string{"node-1", "node-2"} {
		state, err := desiredStateForAgent(ctx, store, agentID)
		if err != nil {
			t.Fatalf("desiredStateForAgent(%s): %v", agentID, err)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv6Pool(); got != "fd00:200::/48" {
			t.Fatalf("agent %s got workload pool %q", agentID, got)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv4Pool(); got != "10.200.0.0/16" {
			t.Fatalf("agent %s got IPv4 workload pool %q", agentID, got)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv4Subnet(); got == "" {
			t.Fatalf("agent %s got empty IPv4 workload subnet", agentID)
		}
		identities := state.GetNodeConfig().GetWorkloadIdentities()
		if len(identities) != 2 {
			t.Fatalf("agent %s expected 2 same-environment identities, got %d", agentID, len(identities))
		}
		seenHosts := map[string]string{}
		for _, identity := range identities {
			if identity.GetEnvironmentId() != services[0].EnvironmentID || identity.GetNetworkIdentity() == 0 ||
				identity.GetWorkloadIpv4() == "" || identity.GetWorkloadIpv6() == "" {
				t.Fatalf("agent %s got invalid tenant identity %+v", agentID, identity)
			}
			seenHosts[identity.GetHostAgentId()] = identity.GetHostIpv6()
		}
		if seenHosts["node-1"] != "fd00:30::10" || seenHosts["node-2"] != "fd00:30::11" {
			t.Fatalf("agent %s got incomplete host identities: %+v", agentID, seenHosts)
		}
	}

	node1BeforeDelete := mustDesiredRevision(t, store, ctx, "node-1")
	node2BeforeDelete := mustDesiredRevision(t, store, ctx, "node-2")
	if err := deleteService(ctx, store, "user-1", services[0].ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	for _, item := range []struct {
		id     string
		before int64
	}{{"node-1", node1BeforeDelete}, {"node-2", node2BeforeDelete}} {
		if got := mustDesiredRevision(t, store, ctx, item.id); got != item.before+1 {
			t.Fatalf("agent %s identity revision was not bumped: got %d want %d", item.id, got, item.before+1)
		}
		state, err := desiredStateForAgent(ctx, store, item.id)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if item.id == "node-1" {
			want = 0
		}
		if got := len(state.GetNodeConfig().GetWorkloadIdentities()); got != want {
			t.Fatalf("agent %s retained deleted workload identity, got %d", item.id, got)
		}
	}
}

func TestDesiredStateForSingleNodeClusterHasNoPeers(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
		t.Fatalf("upsertAgent(node-1): %v", err)
	}

	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-1): %v", err)
	}
	if state.GetNodeConfig() == nil {
		t.Fatal("expected node config in desired state")
	}
	if got := len(state.GetNodeConfig().GetPeers()); got != 0 {
		t.Fatalf("expected no peers for single-node cluster, got %d", got)
	}
}

func testAgentHello(n int) *agentv1.AgentHello {
	id := fmt.Sprintf("node-%d", n)
	return &agentv1.AgentHello{
		AgentId:                 id,
		Name:                    id,
		AdvertiseAddr:           fmt.Sprintf("fd00:30::%x", 0x10+n),
		WireguardPublicKey:      fmt.Sprintf("test-public-key-%d", n),
		WireguardListenPort:     51820 + int32(n),
		WireguardEndpoint:       fmt.Sprintf("192.0.2.%d:%d", 10+n, 51820+n),
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
	}
}
