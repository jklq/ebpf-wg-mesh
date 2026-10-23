package delivery

import (
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"errors"
	"strings"
	"testing"
)

func TestIPv4SubnetAtAllocatesSequentialNonOverlappingPrefixes(t *testing.T) {
	t.Parallel()

	for ordinal, want := range []string{"10.42.0.0/30", "10.42.0.4/30"} {
		got, err := IPv4SubnetAt("10.42.0.0/29", 30, uint64(ordinal))
		if err != nil {
			t.Fatalf("IPv4SubnetAt(%d): %v", ordinal, err)
		}
		if got != want {
			t.Fatalf("IPv4SubnetAt(%d) = %q, want %q", ordinal, got, want)
		}
	}
	if _, err := IPv4SubnetAt("10.42.0.0/29", 30, 2); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected pool exhaustion, got %v", err)
	}
	if _, err := IPv4SubnetAt("10.42.0.1/29", 30, 0); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("expected non-canonical pool rejection, got %v", err)
	}
}

func TestNextIPv4AddressFromSubnetAllocatesSequentiallyAndReusesGaps(t *testing.T) {
	t.Parallel()

	used := map[string]struct{}{"10.42.0.2": {}, "10.42.0.4": {}}
	got, err := nextIPv4AddressFromSubnet("10.42.0.0/29", used)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.42.0.3" {
		t.Fatalf("next address = %q, want gap 10.42.0.3", got)
	}

	used = map[string]struct{}{
		"10.42.0.2": {}, "10.42.0.3": {}, "10.42.0.4": {},
		"10.42.0.5": {}, "10.42.0.6": {},
	}
	if _, err := nextIPv4AddressFromSubnet("10.42.0.0/29", used); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected subnet exhaustion, got %v", err)
	}
}

func TestPlacementBatchReservesCapacityAndUpdatesLoadOrder(t *testing.T) {
	d := &Delivery{store: &persistence{}}
	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 1000, MemoryMebibytes: 64})
	candidates := []placementCandidate{testCandidate("a", "region", "a", 0), testCandidate("b", "region", "b", 0)}
	occupied := make(map[string]struct{})
	for _, want := range []string{"a", "b", "a", "b"} {
		id, err := d.chooseAgentForReplica(candidates, spec, occupied)
		if err != nil || id != want {
			t.Fatalf("agent = %s, %v, want %s", id, err, want)
		}
		reservePlacement(candidates, id, spec)
		occupied[id] = struct{}{}
	}
	if _, err := d.chooseAgentForReplica(candidates, spec, occupied); !errors.Is(err, ErrNoPlacementAvailable) {
		t.Fatalf("overcommitted batch capacity: %v", err)
	}
}

func TestPlacementRejectsUndersizedServiceOnGVisorCandidates(t *testing.T) {
	gvisor := testCandidate("gvisor", "region", "gvisor", 0)
	gvisor.RuntimeCapabilities = append(gvisor.RuntimeCapabilities, "isolation:gvisor")
	runc := testCandidate("runc", "region", "runc", 0)
	runc.RuntimeCapabilities = append(runc.RuntimeCapabilities, "isolation:runc-userns")

	small := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 128})
	if candidateEligible(gvisor, small) {
		t.Fatal("128 MiB service is eligible for a gVisor candidate; the Sentry would starve inside the limit")
	}
	if !candidateEligible(runc, small) {
		t.Fatal("128 MiB service must stay eligible for a runc-userns candidate")
	}
	floor := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 256})
	if !candidateEligible(gvisor, floor) {
		t.Fatal("256 MiB service must be eligible for a gVisor candidate")
	}
	reason := placementFailureReason([]placementCandidate{gvisor}, small, nil)
	if !strings.Contains(reason, "256") || !strings.Contains(reason, "gVisor") {
		t.Fatalf("placement failure reason does not name the gVisor memory floor: %q", reason)
	}
}
