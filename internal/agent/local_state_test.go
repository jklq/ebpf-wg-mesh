package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"go.etcd.io/bbolt"
)

func TestLocalStateCommitsDesiredConfigurationAndCredentialsSeparately(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	desired := testDesiredState(4, 12, "alloc-1")
	desired.Services[0].RegistryUsername = "pull-user"
	desired.Services[0].RegistryPassword = "pull-secret"
	changed, err := store.acceptDesired("cluster-a", desired)
	if err != nil {
		t.Fatalf("accept desired: %v", err)
	}
	if !changed {
		t.Fatal("first accepted desired state was not marked changed")
	}

	restored, err := store.desiredState()
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.GetServices()[0].GetRegistryPassword(); got != "pull-secret" {
		t.Fatalf("restored pull password = %q", got)
	}
	if err := store.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(localDesiredBucket).Get(desiredStateKey)
		if strings.Contains(string(raw), "pull-secret") {
			t.Fatal("credential leaked into accepted desired configuration")
		}
		if !strings.Contains(string(tx.Bucket(localCredentialsBucket).Get([]byte("alloc-1"))), "pull-secret") {
			t.Fatal("credential was not persisted in the protected credential bucket")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state database mode = %o, want 600", got)
	}

	// Credential renewal is accepted at the same cursor and does not look like
	// an allocation configuration change.
	desired.Services[0].RegistryPassword = "renewed-secret"
	changed, err = store.acceptDesired("cluster-a", desired)
	if err != nil {
		t.Fatalf("renew credential: %v", err)
	}
	if changed {
		t.Fatal("credential renewal manufactured an allocation change")
	}
}

func TestLocalStateFencesStaleAuthorityAndCursor(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(3, 10, "alloc-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(2, 99, "alloc-1")); err == nil || !strings.Contains(err.Error(), "stale authority epoch") {
		t.Fatalf("stale epoch error = %v", err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(3, 9, "alloc-1")); err == nil || !strings.Contains(err.Error(), "stale reconciliation cursor") {
		t.Fatalf("stale cursor error = %v", err)
	}
	mutated := testDesiredState(3, 10, "alloc-2")
	if _, err := store.acceptDesired("cluster-a", mutated); err == nil || !strings.Contains(err.Error(), "without advancing") {
		t.Fatalf("same-cursor mutation error = %v", err)
	}
	if _, err := store.acceptDesired("cluster-b", testDesiredState(4, 1, "alloc-1")); err == nil || !strings.Contains(err.Error(), "belongs to cluster") {
		t.Fatalf("cluster identity error = %v", err)
	}
}

func TestLocalStateRecoveryRequiresOwnershipOfEveryDiscoveredResource(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	if err := store.prepareStartup("", []RuntimeResource{
		{AllocationID: "alloc-1", RuntimeID: "platform-alloc-1"},
		{AllocationID: "orphan", RuntimeID: "platform-orphan"},
		{VolumeID: "orphan-volume", RuntimeID: filepath.Join(t.TempDir(), "orphan-volume")},
	}); err != nil {
		t.Fatal(err)
	}
	assertInitializationState(t, store, initializationRecovery)

	if _, err := store.acceptDesired("cluster-a", testDesiredState(1, 1, "alloc-1")); err != nil {
		t.Fatal(err)
	}
	assertInitializationState(t, store, initializationRecovery)

	owned := testDesiredState(1, 2, "alloc-1", "orphan")
	owned.Volumes = append(owned.Volumes, &agentv1.DesiredVolume{VolumeId: "orphan-volume", EnvironmentId: "env-1", Name: "orphan", SizeBytes: 1024})
	if _, err := store.acceptDesired("cluster-a", owned); err != nil {
		t.Fatal(err)
	}
	assertInitializationState(t, store, initializationReady)
}

func TestLocalStateKnownClusterSnapshotLiftsRecoveryFence(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", []RuntimeResource{{AllocationID: "orphan", RuntimeID: "platform-orphan"}}); err != nil {
		t.Fatal(err)
	}
	assertInitializationState(t, store, initializationRecovery)
	if _, err := store.acceptDesired("cluster-a", testDesiredState(1, 1)); err != nil {
		t.Fatal(err)
	}
	assertInitializationState(t, store, initializationReady)
}

func TestCorruptLocalStateIsQuarantinedInRecoveryMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, localStateFileName)
	if err := os.WriteFile(path, []byte("not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatalf("open corrupt local state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	summary, err := store.summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Initialization != initializationRecovery {
		t.Fatalf("initialization = %q, want recovery", summary.Initialization)
	}
	if summary.QuarantinedStore == "" {
		t.Fatal("corrupt database was not quarantined")
	}
	if _, err := os.Stat(summary.QuarantinedStore); err != nil {
		t.Fatalf("stat quarantined database: %v", err)
	}
}

func TestStructurallyValidButCorruptLocalStateIsQuarantined(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(localDesiredBucket).Put(desiredStateKey, []byte{0xff})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatalf("open logically corrupt local state: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	summary, err := recovered.summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Initialization != initializationRecovery || summary.QuarantinedStore == "" {
		t.Fatalf("recovered summary = %+v", summary)
	}
}

func TestMalformedLocalStateFormatIsQuarantined(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(localMetaBucket).Put(formatVersionKey, []byte{1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatalf("open malformed local state: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	summary, err := recovered.summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Initialization != initializationRecovery || summary.QuarantinedStore == "" {
		t.Fatalf("recovered summary = %+v", summary)
	}
}

func TestLocalStatePersistsObservationSequenceAndAppliedGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(1, 7, "alloc-1")); err != nil {
		t.Fatal(err)
	}
	report := &agentv1.StatusReport{AgentId: "node-1", Services: []*agentv1.ServiceCondition{{
		AllocationId: "alloc-1", DesiredSpecRevision: 1, AppliedSpecRevision: 1,
		DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1, Phase: "Healthy", Healthy: true,
	}}}
	first, changed, err := store.recordReport(report)
	if err != nil || !changed || first.GetObservationSequence() != 1 {
		t.Fatalf("first report = %+v, changed=%v, err=%v", first, changed, err)
	}
	if err := store.recordRuntimeInventory([]RuntimeResource{{AllocationID: "alloc-1", RuntimeID: "platform-alloc-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	report.Services[0].Phase = "Starting"
	report.Services[0].Healthy = false
	second, changed, err := reopened.recordReport(report)
	if err != nil || !changed || second.GetObservationSequence() != 2 {
		t.Fatalf("second report = %+v, changed=%v, err=%v", second, changed, err)
	}
	if err := reopened.db.View(func(tx *bbolt.Tx) error {
		allocation, err := readAllocation(tx.Bucket(localAllocationsBucket), "alloc-1")
		if err != nil {
			return err
		}
		if allocation.AppliedGeneration != 1 || allocation.RuntimeID != "platform-alloc-1" {
			t.Fatalf("persisted allocation = %+v", allocation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.acceptDesired("cluster-a", testDesiredState(1, 8)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.recordReport(&agentv1.StatusReport{AgentId: "node-1"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.View(func(tx *bbolt.Tx) error {
		allocation, err := readAllocation(tx.Bucket(localAllocationsBucket), "alloc-1")
		if err != nil {
			return err
		}
		if !allocation.Terminal || allocation.Phase != "Stopped" || allocation.RuntimeID != "" || allocation.PendingOperation != "" {
			t.Fatalf("terminal allocation = %+v", allocation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalStateRetainsDrainOperationUntilTerminalObservation(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	desired := testDesiredState(1, 1, "alloc-1")
	desired.Services[0].Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN
	if _, err := store.acceptDesired("cluster-a", desired); err != nil {
		t.Fatal(err)
	}
	if err := store.recordRuntimeInventory([]RuntimeResource{{AllocationID: "alloc-1", RuntimeID: "platform-alloc-1"}}); err != nil {
		t.Fatal(err)
	}
	report := &agentv1.StatusReport{AgentId: "node-1", Services: []*agentv1.ServiceCondition{{
		AllocationId: "alloc-1", DesiredSpecRevision: 1, AppliedSpecRevision: 1,
		DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1, Phase: "Draining",
	}}}
	if _, _, err := store.recordReport(report); err != nil {
		t.Fatal(err)
	}
	assertAllocationState(t, store, "alloc-1", func(allocation localAllocationState) bool {
		return allocation.PendingOperation == "drain" && !allocation.Terminal && allocation.RuntimeID == "platform-alloc-1"
	})

	report.Services[0].Phase = "Drained"
	if _, _, err := store.recordReport(report); err != nil {
		t.Fatal(err)
	}
	if err := store.recordRuntimeInventory(nil); err != nil {
		t.Fatal(err)
	}
	assertAllocationState(t, store, "alloc-1", func(allocation localAllocationState) bool {
		return allocation.PendingOperation == "" && allocation.Terminal && allocation.RuntimeID == ""
	})
}

type supervisorTestRuntime struct {
	mu        sync.Mutex
	inventory []RuntimeResource
	calls     int
	cleanup   []bool
}

func (r *supervisorTestRuntime) DiscoverRuntimeResources(context.Context) ([]RuntimeResource, error) {
	return append([]RuntimeResource(nil), r.inventory...), nil
}

func (r *supervisorTestRuntime) ReconcileWithCleanup(_ context.Context, state *agentv1.DesiredNodeState, cleanup bool) (*agentv1.StatusReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.cleanup = append(r.cleanup, cleanup)
	report := &agentv1.StatusReport{AgentId: state.GetAgentId()}
	for _, service := range state.GetServices() {
		report.Services = append(report.Services, &agentv1.ServiceCondition{
			AllocationId: service.GetAllocationId(), ServiceId: service.GetServiceId(),
			DesiredSpecRevision: service.GetDesiredSpecRevision(), AppliedSpecRevision: service.GetDesiredSpecRevision(),
			DesiredRolloutGeneration: service.GetDesiredRolloutGeneration(), AppliedRolloutGeneration: service.GetDesiredRolloutGeneration(),
			Phase: "Healthy", Healthy: true,
		})
	}
	return report, nil
}

func (*supervisorTestRuntime) Close() error { return nil }

func TestSupervisorRestoresAndReconcilesWithoutControlPlane(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(1, 3, "alloc-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	runtime := &supervisorTestRuntime{inventory: []RuntimeResource{{AllocationID: "alloc-1", RuntimeID: "platform-alloc-1"}}}
	supervisor := newWorkloadSupervisor("node-1", runtime, restored, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx, "cluster-a"); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	runtime.mu.Lock()
	calls := runtime.calls
	cleanup := append([]bool(nil), runtime.cleanup...)
	runtime.mu.Unlock()
	if calls != 1 || len(cleanup) != 1 || !cleanup[0] {
		t.Fatalf("offline startup reconciles=%d cleanup=%v", calls, cleanup)
	}
	if report, err := supervisor.CurrentReport(); err != nil || report.GetObservationSequence() != 1 {
		t.Fatalf("restored report = %+v, err=%v", report, err)
	}
}

func TestSupervisorDisablesCleanupWhileRuntimeOwnershipIsUnknown(t *testing.T) {
	t.Parallel()

	store := openTestLocalState(t)
	inventory := []RuntimeResource{
		{AllocationID: "alloc-1", RuntimeID: "platform-alloc-1"},
		{AllocationID: "unknown", RuntimeID: "platform-unknown"},
	}
	if err := store.prepareStartup("", inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", testDesiredState(1, 1, "alloc-1")); err != nil {
		t.Fatal(err)
	}
	runtime := &supervisorTestRuntime{inventory: inventory}
	supervisor := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx, "cluster-a"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	cleanup := append([]bool(nil), runtime.cleanup...)
	runtime.mu.Unlock()
	if len(cleanup) != 1 || cleanup[0] {
		t.Fatalf("recovery reconciliation cleanup = %v, want [false]", cleanup)
	}
	if supervisor.Ready() {
		t.Fatal("supervisor reported ready before ownership was established")
	}
}

func openTestLocalState(t *testing.T) *localStateStore {
	t.Helper()
	store, err := openLocalStateStore(t.TempDir(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertInitializationState(t *testing.T, store *localStateStore, want initializationState) {
	t.Helper()
	summary, err := store.summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Initialization != want {
		t.Fatalf("initialization = %q, want %q", summary.Initialization, want)
	}
}

func assertAllocationState(t *testing.T, store *localStateStore, allocationID string, valid func(localAllocationState) bool) {
	t.Helper()
	if err := store.db.View(func(tx *bbolt.Tx) error {
		allocation, err := readAllocation(tx.Bucket(localAllocationsBucket), allocationID)
		if err != nil {
			return err
		}
		if !valid(allocation) {
			t.Fatalf("allocation state = %+v", allocation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testDesiredState(epoch uint64, cursor int64, allocationIDs ...string) *agentv1.DesiredNodeState {
	state := &agentv1.DesiredNodeState{
		AgentId: "node-1", AuthorityEpoch: epoch, ReconciliationCursor: cursor,
		NodeConfig: &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.0.0/24"},
		Volumes:    []*agentv1.DesiredVolume{{VolumeId: "volume-1", EnvironmentId: "env-1", Name: "data", SizeBytes: 1024}},
	}
	for _, allocationID := range allocationIDs {
		state.Services = append(state.Services, &agentv1.DesiredService{
			AllocationId: allocationID, ServiceId: "service-" + allocationID, EnvironmentId: "env-1",
			DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, Intent: agentv1.AllocationIntent_ALLOCATION_INTENT_RUN,
			Spec: &platformv1.ResolvedServiceSpec{Image: "example.test/image@sha256:abc", Runtime: &platformv1.ServiceRuntime{}},
		})
	}
	return state
}
