//go:build integration

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type rolloutIngressProbe struct {
	store     *Store
	err       error
	syncCalls int
	snapshots [][]ingressBackend
}

func (p *rolloutIngressProbe) Sync(ctx context.Context) error {
	p.syncCalls++
	backends, err := p.store.listHealthyIngressBackends(ctx)
	if err != nil {
		return err
	}
	p.snapshots = append(p.snapshots, backends)
	return p.err
}

func (*rolloutIngressProbe) RequestSync() {}

func TestRollingReplacementWaitsForIngressBeforeDrain(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	if _, _, err := store.createDomainBinding(ctx, "user-1", projectID, "web.example.com", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	old := allocationForGeneration(t, store, service.ID, 1)[0]

	next := rollingTestSpec("example.test/web:b", 1, 1)
	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", next); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)
	if len(target) != 1 {
		t.Fatalf("expected one overlapping replacement, got %d", len(target))
	}
	if allocations := mustRolloutAllocations(t, store, service.ID); len(allocations) != 2 {
		t.Fatalf("replacement deleted the active allocation: %+v", allocations)
	}
	markRolloutAllocationReady(t, store, target[0])

	probe := &rolloutIngressProbe{store: store, err: errors.New("caddy unavailable")}
	reconciler := NewRolloutReconciler(store, nil, probe, nil, time.Second)
	fixedNow := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	reconciler.now = func() time.Time { return fixedNow }
	if err := reconciler.Reconcile(ctx); err == nil || !errors.Is(err, probe.err) {
		t.Fatalf("expected ingress convergence failure, got %v", err)
	}
	old = allocationByID(t, store, service.ID, old.ID)
	if old.RolloutState != allocationRolloutWithdrawing || old.DrainDeadline.Valid {
		t.Fatalf("old allocation drained before ingress acknowledgement: %+v", old)
	}
	assertDesiredIntent(t, store, old, false)
	if len(probe.snapshots) != 1 || len(probe.snapshots[0]) != 1 || probe.snapshots[0][0].AllocationID != target[0].ID {
		t.Fatalf("ingress convergence did not contain only the healthy replacement: %+v", probe.snapshots)
	}

	// A new reconciler models control-plane restart. The durable withdrawing
	// state makes it retry Caddy before permitting SIGTERM.
	probe.err = nil
	restarted := NewRolloutReconciler(store, nil, probe, nil, time.Second)
	restarted.now = func() time.Time { return fixedNow }
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	old = allocationByID(t, store, service.ID, old.ID)
	if old.RolloutState != allocationRolloutDraining || !old.DrainDeadline.Valid {
		t.Fatalf("expected durable drain intent after ingress convergence: %+v", old)
	}
	wantDeadline := fixedNow.Add(time.Duration(next.GetRollingStrategy().GetDrainingSeconds()) * time.Second)
	if !old.DrainDeadline.Time.Equal(wantDeadline) {
		t.Fatalf("drain deadline = %v, want %v", old.DrainDeadline.Time, wantDeadline)
	}
	assertDesiredIntent(t, store, old, true)
}

func TestRollingReplacementUsesPlatformManagedSingleReplicaBatches(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 3, 2)
	ctx := context.Background()
	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:b", 3, 2)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	firstBatch := allocationForGeneration(t, store, service.ID, 2)
	if len(firstBatch) != 1 {
		t.Fatalf("first batch = %d, want platform surge 1", len(firstBatch))
	}
	if got := len(mustRolloutAllocations(t, store, service.ID)); got != 4 {
		t.Fatalf("allocation count = %d, want desired 3 + platform surge 1", got)
	}
	for _, alloc := range firstBatch {
		markRolloutAllocationReady(t, store, alloc)
	}
	probe := &rolloutIngressProbe{store: store}
	reconciler := NewRolloutReconciler(store, nil, probe, nil, time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile first batch: %v", err)
	}
	assertServingCount(t, store, service.ID, 3)
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile second batch: %v", err)
	}
	secondBatch := allocationForGeneration(t, store, service.ID, 2)
	if len(secondBatch) != 2 {
		t.Fatalf("target allocations after second batch = %d, want 2", len(secondBatch))
	}
	if got := len(mustRolloutAllocations(t, store, service.ID)); got > 4 {
		t.Fatalf("surge bound exceeded: %d allocations", got)
	}
	for _, alloc := range secondBatch {
		if alloc.RolloutState == allocationRolloutStarting {
			markRolloutAllocationReady(t, store, alloc)
		}
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile second batch: %v", err)
	}
	assertServingCount(t, store, service.ID, 3)
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile final batch: %v", err)
	}
	finalBatch := allocationForGeneration(t, store, service.ID, 2)
	if len(finalBatch) != 3 {
		t.Fatalf("target allocations after final batch = %d, want 3", len(finalBatch))
	}
	for _, alloc := range finalBatch {
		if alloc.RolloutState == allocationRolloutStarting {
			markRolloutAllocationReady(t, store, alloc)
		}
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile final replacement: %v", err)
	}
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("complete rollout: %v", err)
	}
	assertRolloutState(t, store, service.ID, 2, rolloutStateSucceeded, "")
}

func TestRollingReplacementReadinessFailureKeepsHealthyGeneration(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)[0]
	if _, err := store.db.ExecContext(ctx,
		`UPDATE allocations SET phase = 'Error', message = 'readiness HTTP 503', updated_at = now() WHERE id = $1`, target.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.advanceRollout(ctx, service.ID, time.Now().UTC()); err != nil {
		t.Fatalf("advanceRollout: %v", err)
	}
	assertRolloutState(t, store, service.ID, 2, rolloutStateFailed, "readiness HTTP 503")
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	if old.RolloutState != allocationRolloutServing || !old.Healthy {
		t.Fatalf("failed replacement disturbed healthy predecessor: %+v", old)
	}
	failed := allocationByID(t, store, service.ID, target.ID)
	if failed.RolloutState != allocationRolloutDraining || failed.Healthy {
		t.Fatalf("failed replacement was not withdrawn for cleanup: %+v", failed)
	}
}

func TestRollingReplacementShutdownTimeoutRemovesDrainedPredecessor(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)[0]
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	markRolloutAllocationReady(t, store, target)
	fixedNow := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	reconciler := NewRolloutReconciler(store, nil, &rolloutIngressProbe{store: store}, nil, time.Second)
	reconciler.now = func() time.Time { return fixedNow }
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile to drain: %v", err)
	}
	old = allocationByID(t, store, service.ID, old.ID)
	if old.RolloutState != allocationRolloutDraining || !old.DrainDeadline.Valid {
		t.Fatalf("expected drain deadline, got %+v", old)
	}
	reconciler.now = func() time.Time { return old.DrainDeadline.Time }
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after shutdown timeout: %v", err)
	}
	for _, alloc := range mustRolloutAllocations(t, store, service.ID) {
		if alloc.ID == old.ID {
			t.Fatalf("predecessor survived shutdown timeout: %+v", alloc)
		}
	}
	assertRolloutState(t, store, service.ID, 2, rolloutStateSucceeded, "")
}

func TestNewerRolloutKeepsServingReplacementAsPredecessor(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()

	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService(b): %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService(b): %v", err)
	}
	replacement := allocationForGeneration(t, store, service.ID, 2)
	if len(replacement) != 1 {
		t.Fatalf("rollout 2 allocations = %+v, want one", replacement)
	}
	markRolloutAllocationReady(t, store, replacement[0])
	reconciler := NewRolloutReconciler(store, nil, &rolloutIngressProbe{store: store}, nil, time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("promote rollout 2 replacement: %v", err)
	}
	serving := allocationByID(t, store, service.ID, replacement[0].ID)
	if serving.RolloutState != allocationRolloutServing || !allocationReady(serving) {
		t.Fatalf("rollout 2 replacement did not enter service: %+v", serving)
	}

	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:c", 1, 1)); err != nil {
		t.Fatalf("updateService(c): %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService(c): %v", err)
	}

	assertRolloutState(t, store, service.ID, 2, rolloutStateSuperseded, "newer rollout")
	kept := allocationByID(t, store, service.ID, replacement[0].ID)
	if kept.RolloutState != allocationRolloutServing || !allocationReady(kept) {
		t.Fatalf("serving replacement was not preserved as a predecessor: %+v", kept)
	}
	if next := allocationForGeneration(t, store, service.ID, 3); len(next) != 1 || next[0].RolloutState != allocationRolloutStarting {
		t.Fatalf("rollout 3 allocations = %+v, want one starting allocation", next)
	}
}

func TestVolumeBackedServiceRejectsOverlappingRollout(t *testing.T) {
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
	envID := productionEnvironmentID(t, store, projects[0].ID)
	if _, err := store.createVolume(ctx, "user-1", envID, "data", 64<<20, "node-1"); err != nil {
		t.Fatalf("createVolume: %v", err)
	}
	spec := rollingTestSpec("example.test/disk:a", 1, 1)
	spec.Runtime.VolumeName = "data"
	service, err := store.createService(ctx, "user-1", envID, "disk", spec, "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	next := rollingTestSpec("example.test/disk:b", 1, 1)
	next.Runtime.VolumeName = "data"
	if _, _, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "", next); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	_, err = store.redeployService(ctx, "user-1", projects[0].ID, service.ID)
	if !errors.Is(err, errVolumeRollingUnsupported) {
		t.Fatalf("redeploy volume service: got %v, want %v", err, errVolumeRollingUnsupported)
	}
	if got := len(mustRolloutAllocations(t, store, service.ID)); got != 1 {
		t.Fatalf("volume redeploy overlapped allocations: %d", got)
	}
}

func TestRollingReplacementRecoversWhenTargetNodeIsLost(t *testing.T) {
	store, projectID, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	if _, _, err := store.updateService(ctx, "user-1", projectID, service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projectID, service.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)[0]
	originalAgent := target.AgentID
	originalTargetID := target.ID
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = $2`, time.Now().Add(-time.Hour), originalAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.failoverUnhealthyServices(ctx, time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("failoverUnhealthyServices: %v", err)
	}
	lost := allocationByID(t, store, service.ID, originalTargetID)
	if lost.AgentID != originalAgent || lost.RolloutState != allocationRolloutLost || lost.Phase != allocationPhaseUnavailable {
		t.Fatalf("lost target was rewritten instead of marked lost: %+v", lost)
	}
	var replacement allocationRecord
	for _, alloc := range allocationForGeneration(t, store, service.ID, 2) {
		if alloc.ID != originalTargetID && alloc.RolloutState == allocationRolloutStarting && alloc.AgentID != originalAgent {
			replacement = alloc
			break
		}
	}
	if replacement.ID == "" {
		t.Fatalf("node loss did not create a new gen-2 allocation: %+v", mustRolloutAllocations(t, store, service.ID))
	}
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	if !old.Healthy || old.RolloutState != allocationRolloutServing {
		t.Fatalf("node loss during rollout disturbed predecessor: %+v", old)
	}
	markRolloutAllocationReady(t, store, replacement)
	reconciler := NewRolloutReconciler(store, nil, &rolloutIngressProbe{store: store}, nil, time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile recovered target: %v", err)
	}
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("complete recovered rollout: %v", err)
	}
	assertRolloutState(t, store, service.ID, 2, rolloutStateSucceeded, "")
}

func createHealthyRollingService(t *testing.T, replicas, _ int32) (*Store, string, serviceRecord) {
	t.Helper()
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
	for index := 1; index <= 6; index++ {
		hello := agentHello(fmt.Sprintf("node-%d", index))
		hello.AdvertiseAddr = fmt.Sprintf("fd00:30::%d", index)
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatalf("upsertAgent: %v", err)
		}
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", rollingTestSpec("example.test/web:a", replicas, 1), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for _, alloc := range allocationForGeneration(t, store, service.ID, 1) {
		markRolloutAllocationReady(t, store, alloc)
	}
	if _, err := store.advanceRollout(ctx, service.ID, time.Now().UTC()); err != nil {
		t.Fatalf("complete initial rollout: %v", err)
	}
	assertRolloutState(t, store, service.ID, 1, rolloutStateSucceeded, "")
	return store, projects[0].ID, service
}

func rollingTestSpec(image string, replicas, _ int32) *platformv1.ServiceSpec {
	spec := directImageServiceSpec(image, &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8080}), CpuMillis: 100, MemoryMebibytes: 64,
	})
	spec.DesiredReplicaCount = replicaCountPtr(replicas)
	spec.RollingStrategy = &platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: replicaCountPtr(60),
		DrainingSeconds:           replicaCountPtr(15),
	}
	return spec
}

func markRolloutAllocationReady(t *testing.T, store *Store, alloc allocationRecord) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE allocations
		    SET applied_spec_revision = desired_spec_revision,
		        applied_rollout_generation = desired_rollout_generation,
		        phase = 'Healthy', message = '', allocation_ipv6 = $1,
		        healthy_ipv6_ports = $2, healthy = TRUE, updated_at = now()
		  WHERE id = $3`,
		"fd00:200::"+alloc.ID[len(alloc.ID)-4:], []byte("[8080]"), alloc.ID,
	); err != nil {
		t.Fatalf("mark allocation %s ready: %v", alloc.ID, err)
	}
}

func markAllDrainingComplete(t *testing.T, store *Store, serviceID string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE allocations SET phase = 'Drained', message = 'graceful exit', updated_at = now()
		  WHERE service_id = $1 AND rollout_state = $2`, serviceID, allocationRolloutDraining,
	); err != nil {
		t.Fatalf("mark drains complete: %v", err)
	}
}

func mustRolloutAllocations(t *testing.T, store *Store, serviceID string) []allocationRecord {
	t.Helper()
	allocs, err := store.listAllocationsByServiceID(context.Background(), serviceID)
	if err != nil {
		t.Fatalf("list allocations: %v", err)
	}
	return allocs
}

func allocationForGeneration(t *testing.T, store *Store, serviceID string, generation int64) []allocationRecord {
	t.Helper()
	var out []allocationRecord
	for _, alloc := range mustRolloutAllocations(t, store, serviceID) {
		if alloc.DesiredRolloutGeneration == generation {
			out = append(out, alloc)
		}
	}
	return out
}

func allocationByID(t *testing.T, store *Store, serviceID, allocationID string) allocationRecord {
	t.Helper()
	for _, alloc := range mustRolloutAllocations(t, store, serviceID) {
		if alloc.ID == allocationID {
			return alloc
		}
	}
	t.Fatalf("allocation %s not found", allocationID)
	return allocationRecord{}
}

func assertServingCount(t *testing.T, store *Store, serviceID string, want int) {
	t.Helper()
	got := 0
	for _, alloc := range mustRolloutAllocations(t, store, serviceID) {
		if alloc.RolloutState == allocationRolloutServing && allocationReady(alloc) {
			got++
		}
	}
	if got != want {
		t.Fatalf("serving count = %d, want %d", got, want)
	}
}

func assertRolloutState(t *testing.T, store *Store, serviceID string, generation int64, wantState, reasonContains string) {
	t.Helper()
	var state, reason string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT state, failure_reason FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`, serviceID, generation,
	).Scan(&state, &reason); err != nil {
		t.Fatalf("load rollout: %v", err)
	}
	if state != wantState || (reasonContains != "" && !contains(reason, reasonContains)) {
		t.Fatalf("rollout state = %q reason %q, want %q containing %q", state, reason, wantState, reasonContains)
	}
}

func assertDesiredIntent(t *testing.T, store *Store, alloc allocationRecord, draining bool) {
	t.Helper()
	services, err := store.listDesiredServices(context.Background(), alloc.AgentID)
	if err != nil {
		t.Fatalf("listDesiredServices: %v", err)
	}
	for _, service := range services {
		if service.GetAllocationId() != alloc.ID {
			continue
		}
		if got := service.GetIntent().String(); draining != (got == "ALLOCATION_INTENT_DRAIN") {
			t.Fatalf("allocation %s intent = %s, draining=%v", alloc.ID, got, draining)
		}
		return
	}
	t.Fatalf("desired allocation %s not found on %s", alloc.ID, alloc.AgentID)
}

func contains(value, substring string) bool {
	return substring == "" || len(value) >= len(substring) && (value == substring || contains(value[1:], substring))
}
