//go:build integration

package controlplane

import (
	"context"
	"crypto/x509"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
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

	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-b", deliverycore.AgentStateCordoned); err != nil {
		t.Fatalf("cordon node-b: %v", err)
	}
	draining, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-a", deliverycore.AgentStateDraining)
	if err != nil {
		t.Fatalf("drain node-a without alternate capacity: %v", err)
	}
	if !strings.Contains(draining.MaintenanceMessage, "drain paused") {
		t.Fatalf("expected drain interruption message, got %q", draining.MaintenanceMessage)
	}
	if agents := allocationAgentIDs(mustListAllocations(t, store, ctx, service.ID)); agents["node-a"] != 1 || agents["node-b"] != 0 {
		t.Fatalf("drain without capacity moved the replica: %v", agents)
	}

	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-b", deliverycore.AgentStateActive); err != nil {
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
	reconciler := NewRolloutReconciler(newTestDelivery(store, nil, probe, nil), time.Second)
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
	pending, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
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
	cleared, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
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
	pending, err := store.reads.ServiceByID(ctx, "user-1", regionService.ID)
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
	service := NewOpsService(nil, store.fleet, newDelivery(store, nil, nil, nil, nil), nil, revocations)
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
	fleet, err := store.fleet.fleetView(ctx, "ops")
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
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-b", deliverycore.AgentStateCordoned); err != nil {
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
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-a", deliverycore.AgentStateCordoned); err != nil {
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
	if _, err := store.catalog.createScheduledVolume(ctx, "user-1", envID, "data", 64<<20); err != nil {
		t.Fatalf("createScheduledVolume: %v", err)
	}
	spec := replicaSpec(100, 64)
	spec.Runtime.VolumeName = "data"
	service, err := createService(ctx, store, "user-1", envID, "disk", spec, "node-a")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	if _, _, err := newTestDelivery(store, nil, nil, nil).SetAgentLifecycle(ctx, "ops", "node-a", deliverycore.AgentStateDraining); err != nil {
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
