package delivery

import (
	"strings"
	"testing"
)

func TestIPv4SubnetAtAllocatesSequentialNonOverlappingPrefixes(t *testing.T) {
	t.Parallel()

	for ordinal, want := range []string{"10.42.0.0/30", "10.42.0.4/30"} {
		got, err := Ipv4SubnetAt("10.42.0.0/29", 30, uint64(ordinal))
		if err != nil {
			t.Fatalf("ipv4SubnetAt(%d): %v", ordinal, err)
		}
		if got != want {
			t.Fatalf("ipv4SubnetAt(%d) = %q, want %q", ordinal, got, want)
		}
	}
	if _, err := Ipv4SubnetAt("10.42.0.0/29", 30, 2); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected pool exhaustion, got %v", err)
	}
	if _, err := Ipv4SubnetAt("10.42.0.1/29", 30, 0); err == nil || !strings.Contains(err.Error(), "canonical") {
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
