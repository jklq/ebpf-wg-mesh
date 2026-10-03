package delivery

import (
	"context"
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
	"fmt"
	"maps"
	"testing"
	"time"
)

func testService(id string, revision, generation int64) *agentv1.DesiredService {
	return &agentv1.DesiredService{AllocationId: id, ServiceId: "svc-" + id, EnvironmentId: "env-1", DesiredSpecRevision: revision, DesiredRolloutGeneration: generation}
}

func syncFixture(t *testing.T) (*Delivery, *journal.DurableState, *AgentSyncPlan) {
	t.Helper()
	live := startLive(t)
	state := &journal.DurableState{ClusterID: "test", LogIndex: 1,
		Projects:     map[string]journal.Project{"p": {ID: "p", Name: "project"}},
		Environments: map[string]journal.Environment{"e": {ID: "e", ProjectID: "p", Name: "environment", NetworkIdentity: 1}},
		Services:     map[string]journal.ServiceIntent{"s": {ID: "s", Name: "service", EnvironmentID: "e", CurrentSpecRevision: 1}},
		Revisions:    map[string]journal.ServiceRevision{"s/1": {ServiceID: "s", SpecRevision: 1, SpecJSON: []byte(`{"runtime":{"env":{"VALUE":"first"}}}`)}},
		Agents:       map[string]journal.AgentRegistration{"agent": {ID: "agent", DesiredRevision: 1}},
		Deployments:  map[string]journal.Deployment{"d": {ID: "d", ServiceID: "s"}},
		Rollouts:     map[string]journal.Rollout{"s/1": {ServiceID: "s", RolloutGeneration: 1, ImageDigest: "example.test/image@sha256:abc"}},
		Assignments:  map[string]journal.Assignment{},
	}
	for _, id := range []string{"keep", "update", "remove"} {
		state.Assignments[id] = journal.Assignment{ID: id, ServiceID: "s", AgentID: "agent", DeploymentID: "d", DesiredSpecRevision: 1, DesiredRolloutGeneration: 1}
	}
	live.applyTestState(*state)
	d := New(Dependencies{Live: live})
	plan, err := d.PlanAgentSync(context.Background(), AgentSyncRequest{AgentID: "agent", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	d.AgentCheckpointSent(plan.Checkpoint)
	return d, state, plan
}

func applySyncFixture(t *testing.T, d *Delivery, state *journal.DurableState) {
	t.Helper()
	before := d.live.product
	state.LogIndex++
	agent := state.Agents["agent"]
	agent.DesiredRevision++
	state.Agents["agent"] = agent
	batch := journal.Diff(before.DurableState, *state)
	d.live.ApplyProduct(journal.Applied{Projection: journal.NewProjection(*state), Batches: []journal.Batch{batch}})
}

func nextSync(t *testing.T, d *Delivery, previous *AgentSyncPlan) *AgentSyncPlan {
	t.Helper()
	plan, err := d.PlanAgentSync(context.Background(), AgentSyncRequest{AgentID: "agent", BaseRevision: previous.Cursor, OverlayVersion: previous.OverlayVersion})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestAgentSyncPlanStartsUpdatesStopsAndNoOpCursor(t *testing.T) {
	d, state, first := syncFixture(t)
	updated := state.Assignments["update"]
	updated.OperatorRestartNonce = 1
	state.Assignments["update"] = updated
	delete(state.Assignments, "remove")
	added := state.Assignments["keep"]
	added.ID = "add"
	state.Assignments["add"] = added
	applySyncFixture(t, d, state)
	plan := nextSync(t, d, first)
	if plan.Checkpoint != nil || len(plan.Diffs) != 1 {
		t.Fatalf("expected one diff: %+v", plan)
	}
	diff := plan.Diffs[0]
	if len(diff.Starts) != 1 || diff.Starts[0].GetAllocationId() != "add" || len(diff.Updates) != 1 || diff.Updates[0].GetAllocationId() != "update" || len(diff.Stops) != 1 || diff.Stops[0] != "remove" {
		t.Fatalf("unexpected changes: %+v", diff)
	}
	if value := diff.Updates[0].GetSpec().GetRuntime().GetEnv()["VALUE"]; value != "first" {
		t.Fatalf("resolved spec missing from update: %q", value)
	}
	if len(plan.Images) != 3 {
		t.Fatalf("pull image set lost unchanged assignments: %+v", plan.Images)
	}
	// A node-only revision still advances the allocation cursor with an empty patch.
	agent := state.Agents["agent"]
	agent.WireguardListenPort = 51821
	state.Agents["agent"] = agent
	applySyncFixture(t, d, state)
	next := nextSync(t, d, plan)
	if next.Checkpoint != nil || len(next.Diffs) != 1 || len(next.Diffs[0].Starts)+len(next.Diffs[0].Updates)+len(next.Diffs[0].Stops) != 0 {
		t.Fatalf("node-only revision must produce no-op allocation diff: %+v", next)
	}
	if next.NodeConfigVersion == plan.NodeConfigVersion {
		t.Fatal("node config did not change independently")
	}
}

func TestAgentSyncPlanRepairsSameCursorOverlayAndUsesRepairedBaseline(t *testing.T) {
	d, state, first := syncFixture(t)
	if err := d.live.BeginSession("agent", "session", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.live.RecordObservation(AllocationObservation{AgentID: "agent", SessionID: "session", AllocationID: "keep", RolloutGeneration: 1, AppliedGeneration: 1, AppliedSpecRevision: 1, Restart: &platformv1.RestartObservation{RestartCount: 2}, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	repaired := nextSync(t, d, first)
	if repaired.Checkpoint == nil || repaired.Cursor != first.Cursor || repaired.OverlayVersion == first.OverlayVersion {
		t.Fatalf("same-cursor overlay drift needs checkpoint: %+v", repaired)
	}
	d.AgentCheckpointSent(repaired.Checkpoint)
	// A durable nonce update must retain the repaired observation fields.
	a := state.Assignments["keep"]
	a.OperatorRestartNonce = 2
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	plan := nextSync(t, d, repaired)
	if plan.Checkpoint != nil || len(plan.Diffs) != 1 || len(plan.Diffs[0].Updates) != 1 || plan.Diffs[0].Updates[0].GetRestartObservation().GetRestartCount() != 2 {
		t.Fatalf("diff lost repaired overlay: %+v", plan)
	}
}

func TestAgentSyncBehindCursorStillRepairsInvalidInventory(t *testing.T) {
	d, state, first := syncFixture(t)
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	var matching []*agentv1.ServiceCondition
	for _, svc := range first.Checkpoint.GetServices() {
		matching = append(matching, &agentv1.ServiceCondition{AllocationId: svc.GetAllocationId(),
			DesiredSpecRevision: svc.GetDesiredSpecRevision(), DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(), Phase: "Running"})
	}
	for _, test := range []struct {
		name       string
		inventory  []*agentv1.ServiceCondition
		checkpoint bool
	}{
		{"matching", matching, false},
		{"missing accepted allocation", matching[1:], true},
		{"unowned running allocation", append(append([]*agentv1.ServiceCondition(nil), matching...), &agentv1.ServiceCondition{AllocationId: "unowned", Phase: "Running"}), true},
		{"stopped extra", append(append([]*agentv1.ServiceCondition(nil), matching...), &agentv1.ServiceCondition{AllocationId: "unowned", Phase: "Stopped"}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := d.PlanAgentSync(context.Background(), AgentSyncRequest{AgentID: "agent", BaseRevision: first.Cursor,
				OverlayVersion: first.OverlayVersion, CheckInventory: true, Inventory: test.inventory})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Cursor <= first.Cursor || plan.OverlayVersion != first.OverlayVersion {
				t.Fatal("fixture must exercise a behind cursor with an unchanged observation overlay")
			}
			if (plan.Checkpoint != nil) != test.checkpoint {
				t.Fatalf("inventory checkpoint=%v want=%v", plan.Checkpoint != nil, test.checkpoint)
			}
		})
	}
}

func TestAgentSyncPlanBoundsHistoryAndFallsBackAfterReset(t *testing.T) {
	d, state, first := syncFixture(t)
	previous := first
	for i := 0; i < MaxDiffEntriesPerAgent+5; i++ {
		a := state.Assignments["keep"]
		a.OperatorRestartNonce++
		state.Assignments["keep"] = a
		applySyncFixture(t, d, state)
		previous = nextSync(t, d, previous)
		if previous.Checkpoint != nil {
			t.Fatalf("recent cursor unexpectedly required checkpoint at %d", i)
		}
	}
	old := nextSync(t, d, first)
	if old.Checkpoint == nil {
		t.Fatal("compacted cursor should require checkpoint")
	}
	d.live.ApplyProduct(journal.Applied{Projection: journal.NewProjection(*state), Reset: true}) // duplicate prefix must be ignored
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	state.LogIndex++
	agent := state.Agents["agent"]
	agent.DesiredRevision++
	state.Agents["agent"] = agent
	d.live.ApplyProduct(journal.Applied{Projection: journal.NewProjection(*state), Reset: true})
	if plan := nextSync(t, d, previous); plan.Checkpoint == nil {
		t.Fatal("recovery reset must discard diff baseline")
	}
}

func TestAgentSyncPlanOversizedPatchRequiresCheckpoint(t *testing.T) {
	d, state, first := syncFixture(t)
	for i := 0; i < MaxDiffAllocationsPerMessage+10; i++ {
		a := state.Assignments["keep"]
		a.ID = fmt.Sprintf("added-%03d", i)
		state.Assignments[a.ID] = a
	}
	applySyncFixture(t, d, state)
	if plan := nextSync(t, d, first); plan.Checkpoint == nil {
		t.Fatal("oversized patch must fall back to checkpoint")
	}
}

// A pinned volume stays in desired state after its workload stops: absence
// from desired state must never be how data gets deleted. Only an explicit
// destruction removes it, and checkpoint and diff agree at every step.
func TestAgentSyncPinnedVolumeOutlivesWorkloadUntilDestroyed(t *testing.T) {
	d, state, _ := syncFixture(t)
	delete(state.Assignments, "remove")
	delete(state.Assignments, "update")
	revision := state.Revisions["s/1"]
	revision.SpecJSON = []byte(`{"runtime":{"volume":{"volumeName":"data","mountPath":"/var/lib/data"}}}`)
	state.Revisions["s/1"] = revision
	state.Volumes = map[string]journal.Volume{"v": {ID: "v", EnvironmentID: "e", Name: "data", SizeBytes: 64 << 20, AgentID: "agent"}}
	applySyncFixture(t, d, state)
	baseline, err := d.PlanAgentSync(context.Background(), AgentSyncRequest{AgentID: "agent", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Checkpoint.GetServices()) != 1 || baseline.Checkpoint.GetServices()[0].GetVolumeId() != "v" || len(baseline.Checkpoint.GetVolumes()) != 1 {
		t.Fatalf("mounted baseline is incomplete: %+v", baseline.Checkpoint)
	}
	d.AgentCheckpointSent(baseline.Checkpoint)

	checkpointVolumes := func(cursor int64) []*agentv1.DesiredVolume {
		t.Helper()
		plan, err := d.PlanAgentSync(context.Background(), AgentSyncRequest{AgentID: "agent", BaseRevision: cursor, RequireCheckpoint: true})
		if err != nil {
			t.Fatal(err)
		}
		return plan.Checkpoint.GetVolumes()
	}

	a := state.Assignments["keep"]
	a.RolloutState = AllocationRolloutLost
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	stopped := nextSync(t, d, baseline)
	if stopped.Checkpoint != nil || len(stopped.Diffs) != 1 {
		t.Fatalf("expected a retained diff: %+v", stopped)
	}
	if diff := stopped.Diffs[0]; len(diff.GetStops()) != 1 || len(diff.GetVolumeStops())+len(diff.GetVolumeStarts()) != 0 {
		t.Fatalf("stopping the workload must leave its pinned volume alone: %+v", diff)
	}
	if volumes := checkpointVolumes(stopped.Cursor); len(volumes) != 1 || volumes[0].GetDestroy() {
		t.Fatalf("checkpoint after stop = %+v, want the retained volume", volumes)
	}

	delete(state.Volumes, "v")
	state.Destructions = map[string]journal.VolumeDestruction{"v": {VolumeID: "v", AgentID: "agent"}}
	applySyncFixture(t, d, state)
	destroy := nextSync(t, d, stopped)
	if destroy.Checkpoint != nil || len(destroy.Diffs) != 1 {
		t.Fatalf("expected a retained diff: %+v", destroy)
	}
	if starts := destroy.Diffs[0].GetVolumeStarts(); len(starts) != 1 || starts[0].GetVolumeId() != "v" || !starts[0].GetDestroy() {
		t.Fatalf("destruction must arrive as an explicit destroy: %+v", destroy.Diffs[0])
	}
	if volumes := checkpointVolumes(destroy.Cursor); len(volumes) != 1 || !volumes[0].GetDestroy() {
		t.Fatalf("checkpoint during destruction = %+v, want a destroy instruction", volumes)
	}

	state.Destructions = map[string]journal.VolumeDestruction{}
	applySyncFixture(t, d, state)
	done := nextSync(t, d, destroy)
	if done.Checkpoint != nil || len(done.Diffs) != 1 || len(done.Diffs[0].GetVolumeStops()) != 1 {
		t.Fatalf("completed destruction must stop tracking the volume: %+v", done)
	}
	if volumes := checkpointVolumes(done.Cursor); len(volumes) != 0 {
		t.Fatalf("checkpoint after destruction = %+v, want none", volumes)
	}
}

func TestAgentSyncSkippedCallbackResetsHistoryAndWakesChangedAgents(t *testing.T) {
	d, state, first := syncFixture(t)
	watch, cancel := d.live.Watch("agent")
	defer cancel()
	before := d.live.product
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	agent := state.Agents["agent"]
	agent.DesiredRevision++
	state.Agents["agent"] = agent
	state.LogIndex++
	second := journal.NewProjection(*state)
	secondBatch := journal.Diff(before.DurableState, second.DurableState)
	project := state.Projects["p"]
	project.Name = "later unrelated command"
	state.Projects["p"] = project
	state.LogIndex++
	third := journal.NewProjection(*state)
	thirdBatch := journal.Diff(second.DurableState, third.DurableState)
	// The N+1 callback contains the complete prefix but only its own suffix.
	d.live.ApplyProduct(journal.Applied{Projection: third, Batches: []journal.Batch{thirdBatch}})
	select {
	case <-watch:
	default:
		t.Fatal("skipped command's changed agent was not woken")
	}
	if plan := nextSync(t, d, first); plan.Checkpoint == nil || plan.Checkpoint.Services[0].GetAllocationId() != "keep" || plan.Checkpoint.Services[0].GetOperatorRestartNonce() != 1 {
		t.Fatalf("skipped invalidations must repair from the complete latest prefix: %+v", plan)
	}
	d.live.ApplyProduct(journal.Applied{Projection: second, Batches: []journal.Batch{secondBatch}})
	if d.live.product != third {
		t.Fatal("delayed N callback replaced the N+1 projection")
	}
	select {
	case <-watch:
		t.Fatal("delayed older callback published an invalidation")
	default:
	}
}

func TestAgentSyncUnrelatedCommitDoesNotInvalidateCapturedPlan(t *testing.T) {
	d, state, first := syncFixture(t)
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	cursor := state.Agents["agent"].DesiredRevision
	baseline := d.live.allocSync.baseline("agent", cursor)
	before := d.live.product
	state.Projects["foreign"] = journal.Project{ID: "foreign", Name: "unhosted project"}
	state.LogIndex++
	d.live.ApplyProduct(journal.Applied{Projection: journal.NewProjection(*state), Batches: []journal.Batch{journal.Diff(before.DurableState, *state)}})
	services := maps.Clone(baseline.services)
	if !d.live.allocSync.acceptDiff("agent", baseline, cursor, services, baseline.volumes,
		storedDiff{Base: first.Cursor, Target: cursor}) {
		t.Fatal("unrelated commit invalidated a plan for an unchanged agent scope")
	}
	if history, target, ok := d.live.allocSync.diffsFrom("agent", first.Cursor); !ok || target != cursor || len(history) != 1 {
		t.Fatalf("captured plan lost retained history: target=%d ok=%v history=%v", target, ok, history)
	}
}

func TestAgentSyncCommittedChangeRejectsCapturedDiff(t *testing.T) {
	d, state, first := syncFixture(t)
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	cursor := state.Agents["agent"].DesiredRevision
	baseline := d.live.allocSync.baseline("agent", cursor)
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	if d.live.allocSync.acceptDiff("agent", baseline, cursor, baseline.services, baseline.volumes,
		storedDiff{Base: first.Cursor, Target: cursor}) {
		t.Fatal("concurrent committed invalidations accepted a stale rendered patch")
	}
	plan := nextSync(t, d, first)
	if plan.Checkpoint != nil || len(plan.Diffs) != 1 || plan.Diffs[0].GetUpdates()[0].GetOperatorRestartNonce() != 2 {
		t.Fatalf("rejected plan consumed pending invalidations: %+v", plan)
	}
}

func TestAgentCheckpointDelayedCallbackPreservesNewerBaseline(t *testing.T) {
	d, state, first := syncFixture(t)
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	second := nextSync(t, d, first)
	// Sending a newer prefix can finish before an earlier checkpoint callback.
	d.AgentCheckpointSent(first.Checkpoint)
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	third := nextSync(t, d, second)
	if third.Checkpoint != nil || len(third.Diffs) != 1 {
		t.Fatalf("delayed checkpoint discarded a newer diff baseline: %+v", third)
	}
}

func TestAgentCheckpointCommittedWhileSendingRequiresFreshBaseline(t *testing.T) {
	d, state, first := syncFixture(t)
	d.live.allocSync.reset()
	a := state.Assignments["keep"]
	a.OperatorRestartNonce++
	state.Assignments["keep"] = a
	applySyncFixture(t, d, state)
	d.AgentCheckpointSent(first.Checkpoint)
	if plan := nextSync(t, d, first); plan.Checkpoint == nil {
		t.Fatal("checkpoint callback established an older prefix after a concurrent commit")
	}
}

func TestTrackedAgentHistoriesEvictLeastRecentlyUsed(t *testing.T) {
	sync := newAllocSync()
	for i := 0; i <= MaxTrackedAgents; i++ {
		sync.rebase(fmt.Sprintf("agent-%04d", i), &agentv1.DesiredNodeState{ReconciliationCursor: 1})
	}
	if _, _, ok := sync.diffsFrom("agent-0000", 1); ok {
		t.Fatal("oldest history retained after bound")
	}
	if _, _, ok := sync.diffsFrom("agent-0001", 1); !ok {
		t.Fatal("recent history missing")
	}
	sync.rebase("new-agent", &agentv1.DesiredNodeState{ReconciliationCursor: 1})
	if _, _, ok := sync.diffsFrom("agent-0002", 1); ok {
		t.Fatal("touch did not update eviction order")
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
