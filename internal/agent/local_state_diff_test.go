package agent

import (
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/reconciliation"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func testDiffService(id string, revision, generation int64) *agentv1.DesiredService {
	return &agentv1.DesiredService{
		AllocationId: id, ServiceId: "service-" + id, EnvironmentId: "env-1",
		DesiredSpecRevision: revision, DesiredRolloutGeneration: generation,
		Intent: agentv1.AllocationIntent_ALLOCATION_INTENT_RUN,
		Spec:   &platformv1.ResolvedServiceSpec{Image: "example.test/image@sha256:abc", Runtime: &platformv1.ServiceRuntime{}},
	}
}

func testDiff(base, target int64, starts, updates []*agentv1.DesiredService, stops []string) *agentv1.AllocationDiff {
	return &agentv1.AllocationDiff{
		AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
		AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		BaseRevision: base, TargetRevision: target,
		Starts: starts, Updates: updates, Stops: stops,
	}
}

func TestAcceptDiffStartsUpdatesStops(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "keep", "update", "remove")); err != nil {
		t.Fatal(err)
	}
	diff := testDiff(1, 2,
		[]*agentv1.DesiredService{testDiffService("add", 1, 1)},
		[]*agentv1.DesiredService{testDiffService("update", 2, 2)},
		[]string{"remove"})
	changed, err := store.acceptAllocationDiff("cluster-a", "test-session", diff)
	if err != nil || !changed {
		t.Fatalf("accept diff: changed=%v err=%v", changed, err)
	}
	desired, err := store.desiredState()
	if err != nil {
		t.Fatal(err)
	}
	if desired.GetReconciliationCursor() != 2 {
		t.Fatalf("cursor = %d", desired.GetReconciliationCursor())
	}
	ids := make(map[string]*agentv1.DesiredService)
	for _, svc := range desired.GetServices() {
		ids[svc.GetAllocationId()] = svc
	}
	if _, ok := ids["add"]; !ok {
		t.Fatal("start missing")
	}
	if _, ok := ids["remove"]; ok {
		t.Fatal("stop not removed")
	}
	if ids["update"].GetDesiredSpecRevision() != 2 {
		t.Fatalf("update not applied: %+v", ids["update"])
	}
	if _, ok := ids["keep"]; !ok {
		t.Fatal("untouched allocation lost (omission must not remove in diffs)")
	}
	summary, err := store.summary()
	if err != nil || summary.ReconciliationCursor != 2 {
		t.Fatalf("hello cursor not advanced: %+v %v", summary, err)
	}
}

func TestAcceptDiffIdempotentDuplicate(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "a")); err != nil {
		t.Fatal(err)
	}
	diff := testDiff(1, 2, []*agentv1.DesiredService{testDiffService("b", 1, 1)}, nil, nil)
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", diff); err != nil {
		t.Fatal(err)
	}
	changed, err := store.acceptAllocationDiff("cluster-a", "test-session", diff)
	if err != nil || changed {
		t.Fatalf("duplicate diff should be idempotent: changed=%v err=%v", changed, err)
	}
}

func TestAcceptDiffRejectsGapAndStaleEpoch(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(2, 5, "a")); err != nil {
		t.Fatal(err)
	}
	// Gap: base does not match accepted cursor.
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", testDiff(3, 6, nil, nil, nil)); err == nil {
		t.Fatal("gapped diff accepted; checkpoint required")
	}
	// Stale epoch.
	stale := testDiff(5, 6, nil, nil, nil)
	stale.AuthorityEpoch = 1
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", stale); err == nil {
		t.Fatal("stale-epoch diff accepted")
	}
	// Stale target (older than accepted).
	old := testDiff(3, 4, nil, nil, nil)
	old.AuthorityEpoch = 2
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", old); err == nil {
		t.Fatal("stale target accepted")
	}
	// Duplicate of applied range is idempotent.
	dup := testDiff(4, 5, nil, nil, nil)
	dup.AuthorityEpoch = 2
	if changed, err := store.acceptAllocationDiff("cluster-a", "test-session", dup); err != nil || changed {
		t.Fatalf("duplicate should be idempotent: changed=%v err=%v", changed, err)
	}
}

func TestDesiredConfigurationEqualIgnoresListOrder(t *testing.T) {
	t.Parallel()
	// Checkpoints arrive in control-plane assignment order while diff
	// application merges through maps; equality must treat the desired
	// configuration as a set of allocations and volumes.
	left := testDesiredState(1, 5, "alloc-b", "alloc-a")
	right := testDesiredState(1, 5, "alloc-a", "alloc-b")
	if !desiredConfigurationEqual(left, right) {
		t.Fatal("equal configurations in different wire order compare unequal")
	}
	right.Services[0].DesiredSpecRevision = 2
	if desiredConfigurationEqual(left, right) {
		t.Fatal("mutated configuration compares equal")
	}
}

func TestApplyDiffToStateCanonicalizesListOrder(t *testing.T) {
	t.Parallel()
	previous := testDesiredState(1, 5, "alloc-c", "alloc-b", "alloc-a")
	previous.Volumes = []*agentv1.DesiredVolume{
		{VolumeId: "volume-2", EnvironmentId: "env-1", Name: "b", SizeBytes: 2},
		{VolumeId: "volume-1", EnvironmentId: "env-1", Name: "a", SizeBytes: 1},
		{VolumeId: "volume-9", EnvironmentId: "env-1", Name: "gone", SizeBytes: 9},
	}
	diff := testDiff(5, 6,
		[]*agentv1.DesiredService{testDiffService("alloc-d", 1, 1)}, nil, []string{"alloc-a"})
	diff.VolumeStops = []string{"volume-9"}
	diff.VolumeStarts = []*agentv1.DesiredVolume{{VolumeId: "volume-0", EnvironmentId: "env-1", Name: "zero", SizeBytes: 3}}
	merged, err := applyDiffToState(previous, diff)
	if err != nil {
		t.Fatal(err)
	}
	wantServices := []string{"alloc-b", "alloc-c", "alloc-d"}
	if len(merged.GetServices()) != len(wantServices) {
		t.Fatalf("merged services = %d, want %d", len(merged.GetServices()), len(wantServices))
	}
	for i, svc := range merged.GetServices() {
		if svc.GetAllocationId() != wantServices[i] {
			t.Fatalf("merged services[%d] = %q, want canonical order %v", i, svc.GetAllocationId(), wantServices)
		}
	}
	wantVolumes := []string{"volume-0", "volume-1", "volume-2"}
	if len(merged.GetVolumes()) != len(wantVolumes) {
		t.Fatalf("merged volumes = %d, want %d", len(merged.GetVolumes()), len(wantVolumes))
	}
	for i, vol := range merged.GetVolumes() {
		if vol.GetVolumeId() != wantVolumes[i] {
			t.Fatalf("merged volumes[%d] = %q, want canonical order %v", i, vol.GetVolumeId(), wantVolumes)
		}
	}
}

func TestRepairCheckpointAfterDiffsAcceptsAnyWireOrder(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 5, "alloc-c", "alloc-b", "alloc-a")); err != nil {
		t.Fatal(err)
	}
	diff := testDiff(5, 6, []*agentv1.DesiredService{testDiffService("alloc-d", 1, 1)}, nil, []string{"alloc-a"})
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", diff); err != nil {
		t.Fatal(err)
	}
	// A repair checkpoint at the same epoch and cursor carries the equivalent
	// configuration in control-plane assignment order. It must be accepted
	// idempotently instead of tripping the same-cursor mutation guard and
	// closing the sync session.
	repair := testDesiredState(1, 6, "alloc-d", "alloc-c", "alloc-b")
	changed, err := store.acceptDesired("cluster-a", "test-session", repair)
	if err != nil {
		t.Fatalf("repair checkpoint after diffs: %v", err)
	}
	if changed {
		t.Fatal("equivalent repair checkpoint reported a configuration change")
	}
}

func TestDiffRequiresCheckpointBaseline(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", testDiff(0, 1, []*agentv1.DesiredService{testDiffService("a", 1, 1)}, nil, nil)); err == nil {
		t.Fatal("diff without checkpoint baseline accepted")
	}
}

func TestNodeConfigCredentialsReplicasVersionedSeparately(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	checkpoint := testDesiredState(1, 1, "a")
	if _, err := store.acceptDesired("cluster-a", "test-session", checkpoint); err != nil {
		t.Fatal(err)
	}
	before, err := store.summary()
	if err != nil {
		t.Fatal(err)
	}
	// Node config update changes node version but not allocation cursor.
	node := &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.9.0/24", WorkloadIpv6Subnet: "fd00:9::/64"}
	update := &agentv1.NodeConfigUpdate{
		AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
		AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		NodeConfig: node, NodeConfigVersion: reconciliation.HashNodeConfig(node),
	}
	changed, err := store.acceptNodeConfigUpdate("cluster-a", "test-session", update)
	if err != nil || !changed {
		t.Fatalf("node config: changed=%v err=%v", changed, err)
	}
	after, err := store.summary()
	if err != nil {
		t.Fatal(err)
	}
	if after.ReconciliationCursor != before.ReconciliationCursor {
		t.Fatal("node config manufactured an allocation change")
	}
	if after.NodeConfigVersion == before.NodeConfigVersion {
		t.Fatal("node config version did not advance")
	}
	// Credentials update changes only credentials version.
	creds := &agentv1.PullCredentialSet{
		AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
		AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		Credentials: []*agentv1.AllocationCredential{{AllocationId: "a", Username: "u", Password: "p"}},
	}
	creds.CredentialsVersion = reconciliation.HashCredentials(creds.GetCredentials())
	if _, err := store.acceptPullCredentials("cluster-a", "test-session", creds); err != nil {
		t.Fatal(err)
	}
	// Duplicate is idempotent.
	if changed, err := store.acceptPullCredentials("cluster-a", "test-session", creds); err != nil || changed {
		t.Fatalf("duplicate credentials: changed=%v err=%v", changed, err)
	}
	// Replicas update changes only replicas version.
	replicas := &agentv1.ReplicaEndpoints{
		AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
		AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		ReplicaAddresses: []string{"replica-a:9443"},
	}
	replicas.ReplicasVersion = reconciliation.HashReplicas(replicas.GetReplicaAddresses())
	if _, err := store.acceptReplicaEndpoints("cluster-a", "test-session", replicas); err != nil {
		t.Fatal(err)
	}
	final, err := store.summary()
	if err != nil {
		t.Fatal(err)
	}
	if final.ReconciliationCursor != before.ReconciliationCursor || final.NodeConfigVersion != after.NodeConfigVersion {
		t.Fatal("credentials/replicas leaked into allocation or node versions")
	}
	if final.CredentialsVersion == "" || final.ReplicasVersion == "" {
		t.Fatal("independent versions not persisted for hello")
	}
	// Version mismatch fails closed.
	bad := &agentv1.PullCredentialSet{
		AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
		AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		CredentialsVersion: "deadbeef",
		Credentials:        []*agentv1.AllocationCredential{{AllocationId: "a", Username: "u", Password: "p2"}},
	}
	if _, err := store.acceptPullCredentials("cluster-a", "test-session", bad); err == nil {
		t.Fatal("version mismatch accepted")
	}
}

func TestDiffPreservesRecoveryQuarantine(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	// Recovery with an unowned runtime container.
	inventory := []RuntimeResource{{AllocationID: "unowned", RuntimeID: "runtime-unowned"}}
	if err := store.prepareStartup("cluster-a", inventory); err != nil {
		t.Fatal(err)
	}
	// Checkpoint without the unowned allocation keeps recovery (no cleanup).
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "owned")); err != nil {
		t.Fatal(err)
	}
	summary, err := store.summary()
	if err != nil || summary.Initialization != initializationRecovery {
		t.Fatalf("recovery exited with unowned container: %+v %v", summary, err)
	}
	// A diff that still omits the unowned container also preserves recovery.
	diff := testDiff(1, 2, []*agentv1.DesiredService{testDiffService("owned2", 1, 1)}, nil, nil)
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", diff); err != nil {
		t.Fatal(err)
	}
	summary, err = store.summary()
	if err != nil || summary.Initialization != initializationRecovery {
		t.Fatalf("diff exited recovery with unowned container: %+v %v", summary, err)
	}
	// Once runtime inventory only contains owned allocations, recovery exits.
	if err := store.recordRuntimeInventory([]RuntimeResource{{AllocationID: "owned", RuntimeID: "r1"}, {AllocationID: "owned2", RuntimeID: "r2"}}); err != nil {
		t.Fatal(err)
	}
	diff2 := testDiff(2, 3, nil, []*agentv1.DesiredService{testDiffService("owned", 1, 2)}, nil)
	// The update must carry the same allocation identity; adjust generation.
	diff2.Updates[0].DesiredSpecRevision = 1
	diff2.Updates[0].DesiredRolloutGeneration = 1
	if _, err := store.acceptAllocationDiff("cluster-a", "test-session", diff2); err != nil {
		t.Fatal(err)
	}
	summary, err = store.summary()
	if err != nil || summary.Initialization != initializationReady {
		t.Fatalf("recovery did not exit once owned: %+v %v", summary, err)
	}
}

func TestAcceptNoOpDiffAdvancesCursorBeforeNextDiff(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "keep")); err != nil {
		t.Fatal(err)
	}
	// A revision that changes no allocation content (e.g., a peer-only bump
	// of desired_revision) arrives as an empty diff. It must advance the
	// accepted cursor without touching allocations so the next real diff,
	// based on the advanced cursor, is accepted instead of rejected for a
	// base mismatch.
	changed, err := store.acceptAllocationDiff("cluster-a", "test-session", testDiff(1, 2, nil, nil, nil))
	if err != nil || !changed {
		t.Fatalf("accept no-op diff: changed=%v err=%v", changed, err)
	}
	desired, err := store.desiredState()
	if err != nil {
		t.Fatal(err)
	}
	if desired.GetReconciliationCursor() != 2 {
		t.Fatalf("cursor = %d, want 2", desired.GetReconciliationCursor())
	}
	if len(desired.GetServices()) != 1 || desired.GetServices()[0].GetAllocationId() != "keep" {
		t.Fatalf("no-op diff changed allocations: %+v", desired.GetServices())
	}
	changed, err = store.acceptAllocationDiff("cluster-a", "test-session",
		testDiff(2, 3, []*agentv1.DesiredService{testDiffService("next", 1, 1)}, nil, nil))
	if err != nil || !changed {
		t.Fatalf("follow-up diff on the advanced base was not accepted: changed=%v err=%v", changed, err)
	}
	summary, err := store.summary()
	if err != nil || summary.ReconciliationCursor != 3 {
		t.Fatalf("hello cursor not advanced: %+v %v", summary, err)
	}
}

func TestAcceptSameCursorCheckpointUpdatesNodeConfiguration(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 4, "keep")); err != nil {
		t.Fatal(err)
	}
	// Node configuration is an independently versioned stream: a same-cursor
	// recovery or inventory repair checkpoint carries the latest node
	// configuration and must apply it instead of rejecting the repair as a
	// same-cursor mutation and closing the session in a reconnect loop.
	updated := testDesiredState(1, 4, "keep")
	updated.NodeConfig = &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.0.0/24", WireguardListenPort: 51821}
	updated.NodeConfigVersion = reconciliation.HashNodeConfig(updated.GetNodeConfig())
	changed, err := store.acceptDesired("cluster-a", "test-session", updated)
	if err != nil {
		t.Fatalf("same-cursor repair with changed node configuration rejected: %v", err)
	}
	if !changed {
		t.Fatal("node configuration update was not applied as a change")
	}
	desired, err := store.desiredState()
	if err != nil {
		t.Fatal(err)
	}
	if desired.GetNodeConfig().GetWireguardListenPort() != 51821 {
		t.Fatalf("node configuration not updated: %+v", desired.GetNodeConfig())
	}
	if desired.GetReconciliationCursor() != 4 {
		t.Fatalf("cursor = %d, want 4", desired.GetReconciliationCursor())
	}
	// A duplicate of the same checkpoint stays idempotent.
	if changed, err := store.acceptDesired("cluster-a", "test-session", updated); err != nil || changed {
		t.Fatalf("duplicate checkpoint should be idempotent: changed=%v err=%v", changed, err)
	}
	// The allocation guard still holds: cursor-versioned content may not
	// change without advancing the cursor.
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 4, "keep", "extra")); err == nil {
		t.Fatal("same-cursor mutation of allocations accepted")
	}
	// The follow-up diff chains from the unchanged cursor.
	changed, err = store.acceptAllocationDiff("cluster-a", "test-session",
		testDiff(4, 5, []*agentv1.DesiredService{testDiffService("next", 1, 1)}, nil, nil))
	if err != nil || !changed {
		t.Fatalf("follow-up diff after same-cursor repair rejected: changed=%v err=%v", changed, err)
	}
}

func TestDesiredConfigurationEqualIgnoresNodeConfig(t *testing.T) {
	t.Parallel()
	// The cursor versions allocations and volumes; node configuration is
	// independently versioned by content hash and must not make an otherwise
	// identical same-cursor checkpoint compare unequal.
	left := testDesiredState(1, 5, "alloc-a")
	right := testDesiredState(1, 5, "alloc-a")
	right.NodeConfig = &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.9.0/24", WireguardListenPort: 51821}
	right.NodeConfigVersion = "other-version"
	if !desiredConfigurationEqual(left, right) {
		t.Fatal("independently versioned node config compares unequal at the same cursor")
	}
	right.Services[0].DesiredSpecRevision = 2
	if desiredConfigurationEqual(left, right) {
		t.Fatal("mutated allocation content compares equal")
	}
}

func TestDesiredConfigurationEqualIgnoresObservationOverlay(t *testing.T) {
	t.Parallel()
	// The observation overlay (internal hosts, restart observations) derives
	// from live control-plane observations and may change at the same
	// reconciliation cursor; a repair checkpoint must not compare unequal
	// because of it.
	left := testDesiredState(1, 5, "alloc-a")
	right := testDesiredState(1, 5, "alloc-a")
	right.Services[0].InternalHosts = []*agentv1.InternalHost{{Hostname: "alloc-a.mesh.internal", Ipv4: "10.0.0.7"}}
	right.Services[0].RestartObservation = &platformv1.RestartObservation{RestartCount: 2}
	if !desiredConfigurationEqual(left, right) {
		t.Fatal("observation overlay compares unequal at the same cursor")
	}
	right.Services[0].DesiredSpecRevision = 2
	if desiredConfigurationEqual(left, right) {
		t.Fatal("mutated allocation content compares equal")
	}
}

func TestAcceptSameCursorCheckpointUpdatesObservationOverlay(t *testing.T) {
	t.Parallel()
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	baseline := testDesiredState(1, 4, "keep")
	if _, err := store.acceptDesired("cluster-a", "test-session", baseline); err != nil {
		t.Fatal(err)
	}
	baselineOverlay := reconciliation.HashObservationOverlay(baseline.GetServices())
	if summary, err := store.summary(); err != nil || summary.ObservationOverlayVersion != baselineOverlay {
		t.Fatalf("summary overlay = %q want %q: %v", summary.ObservationOverlayVersion, baselineOverlay, err)
	}
	// Observation-derived fields drift without a revision bump (a health
	// change altered internal hosts); a reconnect repair checkpoint carries
	// the refreshed overlay at the same cursor and must apply it instead of
	// rejecting the repair as a same-cursor mutation.
	updated := testDesiredState(1, 4, "keep")
	updated.Services[0].InternalHosts = []*agentv1.InternalHost{{Hostname: "keep.mesh.internal", Ipv4: "10.0.0.9"}}
	changed, err := store.acceptDesired("cluster-a", "test-session", updated)
	if err != nil {
		t.Fatalf("same-cursor repair with changed observation overlay rejected: %v", err)
	}
	if !changed {
		t.Fatal("observation overlay update was not applied as a change")
	}
	desired, err := store.desiredState()
	if err != nil {
		t.Fatal(err)
	}
	if hosts := desired.GetServices()[0].GetInternalHosts(); len(hosts) != 1 || hosts[0].GetIpv4() != "10.0.0.9" {
		t.Fatalf("observation overlay not updated: %+v", hosts)
	}
	updatedOverlay := reconciliation.HashObservationOverlay(updated.GetServices())
	if summary, err := store.summary(); err != nil || summary.ObservationOverlayVersion != updatedOverlay {
		t.Fatalf("summary overlay after repair = %q want %q: %v", summary.ObservationOverlayVersion, updatedOverlay, err)
	}
	// A duplicate of the same checkpoint stays idempotent.
	if changed, err := store.acceptDesired("cluster-a", "test-session", updated); err != nil || changed {
		t.Fatalf("duplicate checkpoint should be idempotent: changed=%v err=%v", changed, err)
	}
	// The allocation guard still holds: cursor-versioned content may not
	// change without advancing the cursor.
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 4, "keep", "extra")); err == nil {
		t.Fatal("same-cursor mutation of allocations accepted")
	}
	// The follow-up diff chains from the unchanged cursor.
	changed, err = store.acceptAllocationDiff("cluster-a", "test-session",
		testDiff(4, 5, []*agentv1.DesiredService{testDiffService("next", 1, 1)}, nil, nil))
	if err != nil || !changed {
		t.Fatalf("follow-up diff after same-cursor repair rejected: changed=%v err=%v", changed, err)
	}
}
