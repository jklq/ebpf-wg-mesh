package delivery

import (
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestValidateAgentTransition(t *testing.T) {
	t.Parallel()

	allowed := [][2]AgentLifecycleState{
		{AgentStateActive, AgentStateCordoned},
		{AgentStateActive, AgentStateDraining},
		{AgentStateCordoned, AgentStateActive},
		{AgentStateCordoned, AgentStateDraining},
		{AgentStateCordoned, AgentStateRetired},
		{AgentStateDraining, AgentStateActive},
		{AgentStateDraining, AgentStateCordoned},
		{AgentStateDraining, AgentStateRetired},
		{AgentStateEnrolling, AgentStateRetired},
		{AgentStateUnavailable, AgentStateRetired},
	}
	for _, pair := range allowed {
		if err := validateAgentTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("%s -> %s: %v", pair[0], pair[1], err)
		}
	}
	blocked := [][2]AgentLifecycleState{
		{AgentStateActive, AgentStateRetired},
		{AgentStateEnrolling, AgentStateActive},
		{AgentStateUnavailable, AgentStateActive},
		{AgentStateRetired, AgentStateActive},
	}
	for _, pair := range blocked {
		if err := validateAgentTransition(pair[0], pair[1]); err == nil {
			t.Fatalf("%s -> %s: expected invalid transition", pair[0], pair[1])
		}
	}
}

func TestReplicaPlacementSpreadsAcrossFailureDomainsBeforeColocating(t *testing.T) {
	t.Parallel()

	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 64})
	candidates := []placementCandidate{
		testCandidate("node-a", "us-east", "zone-1", 0),
		testCandidate("node-b", "us-east", "zone-1", 0),
		testCandidate("node-c", "us-east", "zone-2", 0),
	}
	occupied := map[string]struct{}{}
	first := firstEligibleReplicaAgent(candidates, spec, nil, occupied, true, true)
	if first != "node-a" {
		t.Fatalf("first replica = %q, want node-a", first)
	}
	occupied[first] = struct{}{}
	second := firstEligibleReplicaAgent(candidates, spec, nil, occupied, true, true)
	if second != "node-c" {
		t.Fatalf("second replica = %q, want node-c in a different failure domain", second)
	}
}

func TestReplicaPlacementRespectsRegionAndExplainsCordonedCapacity(t *testing.T) {
	t.Parallel()

	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 64})
	spec.PlacementRegion = "eu-west"
	candidates := []placementCandidate{
		testCandidate("node-a", "us-east", "zone-1", 0),
	}
	if got := firstEligibleReplicaAgent(candidates, spec, nil, nil, true, true); got != "" {
		t.Fatalf("placed on %q despite region mismatch", got)
	}
	reason := placementFailureReason(candidates, spec, nil)
	if !strings.Contains(reason, `region "eu-west"`) {
		t.Fatalf("expected region pending reason, got %q", reason)
	}
	if reason := placementFailureReason(nil, spec, nil); !strings.Contains(reason, "cordoned") {
		t.Fatalf("expected empty-candidate reason to mention cordoned nodes, got %q", reason)
	}
}

func testCandidate(id, region, domain string, usedCPU int64) placementCandidate {
	return placementCandidate{
		ID:                     id,
		Region:                 region,
		FailureDomain:          domain,
		RuntimeCapabilities:    []string{"containerd", "wireguard", "ebpf-policy"},
		CPUMillisCapacity:      2000,
		MemoryMebibytesCapcity: 4096,
		UsedCPUMillis:          usedCPU,
	}
}

func TestFailoverDecision(t *testing.T) {
	base := failoverSnapshot{Allocation: policyAllocation("dead", AllocationRolloutServing, 2, time.Now()), DeadAgentID: "dead", ReusableImage: true, Generation: 2}
	for _, tc := range []struct {
		name   string
		change func(*failoverSnapshot)
		action failoverAction
	}{
		{"replace", func(s *failoverSnapshot) {}, failoverReplace},
		{"different agent", func(s *failoverSnapshot) { s.DeadAgentID = "other" }, failoverIgnore},
		{"already lost", func(s *failoverSnapshot) { s.Allocation.RolloutState = AllocationRolloutLost }, failoverIgnore},
		{"withdrawal", func(s *failoverSnapshot) {
			s.Allocation.RolloutState = AllocationRolloutWithdrawing
		}, failoverFinishDrain},
		{"drain", func(s *failoverSnapshot) {
			s.Allocation.RolloutState = AllocationRolloutDraining
		}, failoverFinishDrain},
		{"managed", func(s *failoverSnapshot) { s.ProjectKind = ProjectKindManaged }, failoverBlocked},
		{"volume", func(s *failoverSnapshot) { s.VolumeName = "data" }, failoverBlocked},
		{"no capacity", func(s *failoverSnapshot) { s.PlacementFailure = "no capacity" }, failoverBlocked},
		{"no image", func(s *failoverSnapshot) { s.ReusableImage = false }, failoverBlocked},
		{"pending change", func(s *failoverSnapshot) { s.PendingChanges = true }, failoverIgnore},
		{"pending managed change", func(s *failoverSnapshot) { s.PendingChanges = true; s.ProjectKind = ProjectKindManaged }, failoverIgnore},
		{"active target", func(s *failoverSnapshot) { s.RolloutState = rolloutStateInProgress }, failoverAdvance},
		{"active predecessor", func(s *failoverSnapshot) {
			s.RolloutState = rolloutStateInProgress
			s.Generation = 3
		}, failoverIgnore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.change(&s)
			d := decideFailover(s)
			if d.Action != tc.action || (d.Action == failoverBlocked && d.Message == "") {
				t.Fatalf("decision: %+v", d)
			}
		})
	}
}
