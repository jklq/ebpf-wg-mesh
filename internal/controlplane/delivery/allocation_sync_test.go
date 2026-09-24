package delivery

import (
	"fmt"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/reconciliation"
)

func testService(id string, revision, generation int64) *agentv1.DesiredService {
	return &agentv1.DesiredService{
		AllocationId: id, ServiceId: "svc-" + id, EnvironmentId: "env-1",
		DesiredSpecRevision: revision, DesiredRolloutGeneration: generation,
		Intent: agentv1.AllocationIntent_ALLOCATION_INTENT_RUN,
		Spec:   &platformv1.ResolvedServiceSpec{Image: "example.test/img@sha256:abc"},
	}
}

func testCheckpoint(revision int64, services ...*agentv1.DesiredService) *agentv1.DesiredNodeState {
	return &agentv1.DesiredNodeState{
		AgentId: "agent-1", ReconciliationCursor: revision,
		Services: services,
		Volumes:  []*agentv1.DesiredVolume{{VolumeId: "vol-1", EnvironmentId: "env-1", Name: "data", SizeBytes: 1}},
		NodeConfig: &agentv1.AssignedNodeConfig{
			WorkloadIpv4Subnet: "10.0.0.0/24", WorkloadIpv6Subnet: "fd00::/64",
		},
	}
}

func TestDiffSnapshotsStartUpdateStop(t *testing.T) {
	t.Parallel()
	old := testCheckpoint(5, testService("keep", 1, 1), testService("update", 1, 1), testService("remove", 1, 1))
	updated := testService("update", 2, 2)
	new := testCheckpoint(6, testService("keep", 1, 1), updated, testService("add", 1, 1))
	diff := diffSnapshots(stripForDiff(old), stripForDiff(new), 5, 6)
	if diff.Base != 5 || diff.Target != 6 {
		t.Fatalf("revisions = %d->%d", diff.Base, diff.Target)
	}
	if len(diff.Starts) != 1 || diff.Starts[0].GetAllocationId() != "add" {
		t.Fatalf("starts = %+v", diff.Starts)
	}
	if len(diff.Updates) != 1 || diff.Updates[0].GetAllocationId() != "update" || diff.Updates[0].GetDesiredSpecRevision() != 2 {
		t.Fatalf("updates = %+v", diff.Updates)
	}
	if len(diff.Stops) != 1 || diff.Stops[0] != "remove" {
		t.Fatalf("stops = %+v", diff.Stops)
	}
	if len(diff.VolumeStarts) != 0 || len(diff.VolumeStops) != 0 {
		t.Fatalf("volumes changed without cause: %+v %+v", diff.VolumeStarts, diff.VolumeStops)
	}
}

func TestDiffStripsCredentials(t *testing.T) {
	t.Parallel()
	a := testService("a", 1, 1)
	a.RegistryUsername = "user"
	a.RegistryPassword = "secret"
	b := testService("a", 1, 1)
	diff := diffSnapshots(stripForDiff(testCheckpoint(1, a)), stripForDiff(testCheckpoint(2, b)), 1, 2)
	if len(diff.Starts)+len(diff.Updates)+len(diff.Stops) != 0 {
		t.Fatalf("credential-only change produced allocation diff: %+v", diff)
	}
}

func TestRecordAndDiffOrderedAndBounded(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	first := testCheckpoint(1, testService("a", 1, 1))
	if _, ok := sync.recordAndDiff("agent-1", first, 0); ok {
		t.Fatal("uninitialized base should require checkpoint")
	}
	if diffs, ok := sync.recordAndDiff("agent-1", first, 1); !ok || len(diffs) != 0 {
		t.Fatalf("same revision should send nothing: %+v %v", diffs, ok)
	}
	second := testCheckpoint(2, testService("a", 1, 1), testService("b", 1, 1))
	diffs, ok := sync.recordAndDiff("agent-1", second, 1)
	if !ok || len(diffs) != 1 || diffs[0].Base != 1 || diffs[0].Target != 2 || len(diffs[0].Starts) != 1 {
		t.Fatalf("incremental diff missing: %+v %v", diffs, ok)
	}
	// Reconnect at same revision sends nothing (no full resend).
	if diffs, ok := sync.recordAndDiff("agent-1", second, 2); !ok || len(diffs) != 0 {
		t.Fatalf("unchanged reconnect should send nothing: %+v %v", diffs, ok)
	}
}

func TestCompactedHistoryRequiresCheckpoint(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	base := testCheckpoint(0)
	if _, ok := sync.recordAndDiff("agent-1", base, 0); !ok {
		t.Fatal("init should succeed")
	}
	// Push more revisions than retained history.
	for i := int64(1); i <= MaxDiffEntriesPerAgent+5; i++ {
		cur := testCheckpoint(i, testService("a", 1, i))
		if _, ok := sync.recordAndDiff("agent-1", cur, i-1); !ok {
			t.Fatalf("revision %d should diff", i)
		}
	}
	if _, ok := sync.recordAndDiff("agent-1", testCheckpoint(MaxDiffEntriesPerAgent+5, testService("a", 1, MaxDiffEntriesPerAgent+5)), 1); ok {
		t.Fatal("compacted cursor should require checkpoint")
	}
	latest := int64(MaxDiffEntriesPerAgent + 5)
	if diffs, ok := sync.recordAndDiff("agent-1", testCheckpoint(latest, testService("a", 1, latest)), latest-1); !ok || len(diffs) == 0 {
		t.Fatal("recent cursor should still diff")
	}
}

func TestFailoverLosesHistoryAndFallsBackToCheckpoint(t *testing.T) {
	t.Parallel()
	old := newAllocSync()
	if _, ok := old.recordAndDiff("agent-1", testCheckpoint(1, testService("a", 1, 1)), 1); !ok {
		t.Fatal("init")
	}
	if _, ok := old.recordAndDiff("agent-1", testCheckpoint(2, testService("a", 1, 1), testService("b", 1, 1)), 1); !ok {
		t.Fatal("diff")
	}
	fresh := newAllocSync()
	fresh.reset()
	cur := testCheckpoint(2, testService("a", 1, 1), testService("b", 1, 1))
	if _, ok := fresh.recordAndDiff("agent-1", cur, 1); ok {
		t.Fatal("failover with lost history should require checkpoint")
	}
	// Unchanged reconnect after failover sends nothing once baseline exists.
	if _, ok := fresh.recordAndDiff("agent-1", cur, 2); !ok {
		t.Fatal("established cursor should send nothing")
	}
}

func TestOversizedDiffFallsBackToCheckpoint(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	if _, ok := sync.recordAndDiff("agent-1", testCheckpoint(0), 0); !ok {
		t.Fatal("init")
	}
	var services []*agentv1.DesiredService
	for i := 0; i < MaxDiffAllocationsPerMessage+10; i++ {
		id := "alloc-" + strings.Repeat("x", 2) + string(rune('a'+i/26)) + string(rune('A'+i%26)) + strings.Repeat("y", 2)
		svc := testService(id, 1, 1)
		services = append(services, svc)
	}
	cur := testCheckpoint(1, services...)
	if _, ok := sync.recordAndDiff("agent-1", cur, 0); ok {
		t.Fatal("oversized diff should require checkpoint")
	}
}

func TestInventoriesMatch(t *testing.T) {
	t.Parallel()
	current := []*agentv1.DesiredService{testService("a", 1, 2), testService("b", 3, 1)}
	hello := []*agentv1.ServiceCondition{
		{AllocationId: "a", DesiredSpecRevision: 1, DesiredRolloutGeneration: 2, Phase: "Running"},
		{AllocationId: "b", DesiredSpecRevision: 3, DesiredRolloutGeneration: 1, Phase: "Running"},
		{AllocationId: "old", DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, Phase: "Stopped"},
	}
	if !InventoriesMatch(hello, current) {
		t.Fatal("matching inventory with stopped extra should match")
	}
	hello[0].DesiredRolloutGeneration = 1
	if InventoriesMatch(hello, current) {
		t.Fatal("generation mismatch should not match")
	}
	hello[0].DesiredRolloutGeneration = 2
	hello = append(hello, &agentv1.ServiceCondition{AllocationId: "extra", DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, Phase: "Running"})
	if InventoriesMatch(hello, current) {
		t.Fatal("non-stopped extra should force checkpoint repair")
	}
}

func TestIndependentVersionsChangeSeparately(t *testing.T) {
	t.Parallel()
	nodeA := &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.0.0/24"}
	nodeB := &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.1.0/24"}
	if reconciliation.HashNodeConfig(nodeA) == "" || reconciliation.HashNodeConfig(nodeA) == reconciliation.HashNodeConfig(nodeB) {
		t.Fatal("node config versions should differ on content change")
	}
	credsA := []*agentv1.AllocationCredential{{AllocationId: "a", Username: "u", Password: "p1"}}
	credsB := []*agentv1.AllocationCredential{{AllocationId: "a", Username: "u", Password: "p2"}}
	if reconciliation.HashCredentials(credsA) == reconciliation.HashCredentials(credsB) {
		t.Fatal("credential rotation should change version")
	}
	if reconciliation.HashReplicas([]string{"b:1", "a:1"}) != reconciliation.HashReplicas([]string{"a:1", "b:1"}) {
		t.Fatal("replica version should be order-independent")
	}
	if reconciliation.HashReplicas([]string{"a:1"}) == reconciliation.HashReplicas([]string{"a:1", "b:1"}) {
		t.Fatal("replica change should change version")
	}
}

func TestZeroContentRevisionBumpReturnsNoOpCursorDiff(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	sync.recordCurrent("agent-1", testCheckpoint(1, testService("a", 1, 1)))
	// A peer-only bump must surface as an empty no-op diff so the accepted
	// cursor advances in lockstep with the sent position.
	sync.recordCurrent("agent-1", testCheckpoint(2, testService("a", 1, 1)))
	diffs, target, ok := sync.diffsFrom("agent-1", 1)
	if !ok || target != 2 || len(diffs) != 1 {
		t.Fatalf("zero-content bump must return a covering no-op diff: %+v target=%d ok=%v", diffs, target, ok)
	}
	d := diffs[0]
	if d.Base != 1 || d.Target != 2 {
		t.Fatalf("no-op diff revisions = %d->%d, want 1->2", d.Base, d.Target)
	}
	if len(d.Starts)+len(d.Updates)+len(d.Stops)+len(d.VolumeStarts)+len(d.VolumeStops) != 0 {
		t.Fatalf("no-op diff changed allocations: %+v", d)
	}
}

func TestRebaseAfterCheckpointDiffsCarryRestoredOverlay(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	sync.recordCurrent("agent-1", testCheckpoint(1, testService("a", 1, 1)))
	// A same-cursor repair checkpoint changes the baseline; without rebasing,
	// the next diff would compare against the pre-repair snapshot.
	repaired := testService("a", 1, 1)
	repaired.InternalHosts = []*agentv1.InternalHost{{Hostname: "a.mesh.internal", Ipv4: "10.0.0.1"}}
	sync.rebase("agent-1", testCheckpoint(1, repaired))
	// The covering diff must carry the reverted overlay instead of skipping it.
	sync.recordCurrent("agent-1", testCheckpoint(2, testService("a", 1, 1)))
	diffs, target, ok := sync.diffsFrom("agent-1", 1)
	if !ok || target != 2 || len(diffs) != 1 {
		t.Fatalf("restored overlay must be covered by the diff chain: %+v target=%d ok=%v", diffs, target, ok)
	}
	d := diffs[0]
	if len(d.Updates) != 1 || d.Updates[0].GetAllocationId() != "a" {
		t.Fatalf("restored overlay must surface as an allocation update: %+v", d)
	}
	if hosts := d.Updates[0].GetInternalHosts(); len(hosts) != 0 {
		t.Fatalf("update must carry the restored overlay: %+v", hosts)
	}
}

func TestRebaseKeepsNoOpCursorDiffForPeerOnlyBump(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	sync.recordCurrent("agent-1", testCheckpoint(1, testService("a", 1, 1)))
	sync.rebase("agent-1", testCheckpoint(1, testService("a", 1, 1)))
	// A peer-only bump after a rebased baseline is still an empty no-op diff.
	sync.recordCurrent("agent-1", testCheckpoint(2, testService("a", 1, 1)))
	diffs, target, ok := sync.diffsFrom("agent-1", 1)
	if !ok || target != 2 || len(diffs) != 1 {
		t.Fatalf("zero-content bump must return a covering no-op diff: %+v target=%d ok=%v", diffs, target, ok)
	}
	if d := diffs[0]; len(d.Starts)+len(d.Updates)+len(d.Stops)+len(d.VolumeStarts)+len(d.VolumeStops) != 0 {
		t.Fatalf("no-op diff changed allocations: %+v", d)
	}
}

func TestTrackedAgentHistoriesAreBoundedAndEvictLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	sync := newAllocSync()
	total := MaxTrackedAgents + 1
	for i := 0; i < total; i++ {
		agent := fmt.Sprintf("agent-%04d", i)
		sync.recordCurrent(agent, testCheckpoint(1, testService("a", 1, 1)))
	}
	if len(sync.history) != MaxTrackedAgents {
		t.Fatalf("tracked agents = %d, want bound %d", len(sync.history), MaxTrackedAgents)
	}
	// The least-recently used entry is evicted and falls back to checkpoints.
	if _, _, ok := sync.diffsFrom("agent-0000", 1); ok {
		t.Fatal("evicted agent must fall back to checkpoint delivery")
	}
	// A touch refreshes an entry's eviction order.
	sync.recordCurrent("agent-0001", testCheckpoint(1, testService("a", 1, 1)))
	sync.recordCurrent("agent-new", testCheckpoint(1, testService("a", 1, 1)))
	if _, _, ok := sync.diffsFrom("agent-0002", 1); ok {
		t.Fatal("next-oldest agent must be evicted after the refresh")
	}
	sync.recordCurrent("agent-0001", testCheckpoint(2, testService("a", 1, 1)))
	diffs, target, ok := sync.diffsFrom("agent-0001", 1)
	if !ok || target != 2 || len(diffs) != 1 {
		t.Fatalf("refreshed agent must keep its history: %+v target=%d ok=%v", diffs, target, ok)
	}
}
