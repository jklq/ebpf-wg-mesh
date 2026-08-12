package controlplane

import "testing"

func TestInternalServiceHostnameUsesTheMemorableServiceName(t *testing.T) {
	t.Parallel()

	if got := internalServiceHostname("Accurate Reflection", "service-1"); got != "accurate-reflection.mesh.internal" {
		t.Fatalf("unexpected internal hostname %q", got)
	}
	if got := internalServiceShortName("---", "9D6D-6A8E"); got != "service-9d6d6a8e" {
		t.Fatalf("unexpected fallback short name %q", got)
	}
}
