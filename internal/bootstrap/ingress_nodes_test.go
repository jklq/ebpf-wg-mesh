package bootstrap

import (
	"strings"
	"testing"
)

func TestIngressRetirementRequiresTrafficRemovalAssertion(t *testing.T) {
	for _, args := range [][]string{
		{"retire", "--db-url", "postgres://unreachable", "--node-id", "envoy-1"},
		{"retire", "--db-url", "postgres://unreachable", "--traffic-stopped"},
	} {
		if err := RunIngressNodes(args); err == nil || !strings.Contains(err.Error(), "--traffic-stopped") {
			t.Fatalf("retire %v = %v", args, err)
		}
	}
}
