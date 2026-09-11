//go:build integration

package controlplane

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

// assertLiveAllocationsMatchDurable guards the invariant that the owner's
// in-memory allocation view is exactly the durable allocation set after every
// rollout mutation. A violation means live reads drift from the database.
func assertLiveAllocationsMatchDurable(t *testing.T, store *persistence, serviceID string) int {
	t.Helper()
	live := store.liveImplementation.AllocationsByService(serviceID)
	liveIDs := make([]string, 0, len(live))
	for _, alloc := range live {
		liveIDs = append(liveIDs, alloc.ID)
	}
	rows, err := store.db.QueryContext(context.Background(), `SELECT id FROM allocation_assignments WHERE service_id = $1 ORDER BY id`, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	durableIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		durableIDs = append(durableIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(liveIDs)
	sort.Strings(durableIDs)
	if !reflect.DeepEqual(liveIDs, durableIDs) {
		t.Fatalf("live allocations %v != durable allocations %v", liveIDs, durableIDs)
	}
	return len(liveIDs)
}

func TestOwnerLiveAllocationsMatchDurableAcrossRolloutMutations(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()

	// create + release + initial rollout completion
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations after create = %d, want 1", count)
	}

	// rolling replacement: a new allocation is added and the predecessor withdrawn
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 2 {
		t.Fatalf("allocations after release = %d, want 2 (overlap)", count)
	}

	target := allocationForGeneration(t, store, service.ID, 2)
	for _, alloc := range target {
		markRolloutAllocationReady(t, store, alloc)
	}
	probe := &rolloutIngressProbe{store: store}
	reconciler := NewRolloutReconciler(newTestDelivery(store, nil, probe, nil), time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile replacement: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 2 {
		t.Fatalf("allocations after promote = %d, want 2 (withdrawing predecessor)", count)
	}

	// drain completion for the withdrawn predecessor
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile drain: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations after drain = %d, want 1", count)
	}

	// delete marks the service for removal
	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	assertLiveAllocationsMatchDurable(t, store, service.ID)
}
