package delivery

import (
	"strings"
	"testing"

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
