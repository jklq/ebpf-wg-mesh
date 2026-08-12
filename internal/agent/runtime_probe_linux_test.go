//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestLinuxProbeDoesNotFallBackWhenWorkloadNamespaceIsMissing(t *testing.T) {
	t.Parallel()

	result := probeServiceHealthInNamespace(context.Background(), "", "fd00::10", &agentv1.DesiredService{
		Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{
			Ports:       []*platformv1.ServiceRuntimePort{{Port: 8080}},
			HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
		}},
	})
	if result.healthy || !strings.Contains(result.failureReason, "workload network namespace is unavailable") {
		t.Fatalf("expected fail-closed namespace error, got %+v", result)
	}
}
