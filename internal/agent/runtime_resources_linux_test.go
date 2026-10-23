//go:build linux

package agent

import (
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/isolation"
)

func TestEffectiveRuntimeResourcesDefaultLegacyZeroValues(t *testing.T) {
	t.Parallel()

	if got := effectiveCPUMillis(&platformv1.ServiceRuntime{}); got != defaultServiceCPUMillis {
		t.Fatalf("unexpected default CPU request %d", got)
	}
	if got := effectiveMemoryMebibytes(&platformv1.ServiceRuntime{}); got != defaultServiceMemoryMebibytes {
		t.Fatalf("unexpected default memory request %d", got)
	}

	runtime := &platformv1.ServiceRuntime{CpuMillis: 750, MemoryMebibytes: 1024}
	if got := effectiveCPUMillis(runtime); got != 750 {
		t.Fatalf("unexpected explicit CPU request %d", got)
	}
	if got := effectiveMemoryMebibytes(runtime); got != 1024 {
		t.Fatalf("unexpected explicit memory request %d", got)
	}
}

func TestSpecOptsRejectUndersizedServiceOnGVisor(t *testing.T) {
	svc := &agentv1.DesiredService{AllocationId: "alloc-1", EnvironmentId: "env-1", Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{CpuMillis: 250, MemoryMebibytes: 64}}}

	gvisor, _ := isolation.NewPolicy(isolation.ClassGVisor)
	if _, err := (&containerdEngine{isolation: gvisor}).specOpts(svc, nil, "", ""); err == nil || !strings.Contains(err.Error(), "Sentry") {
		t.Fatalf("gVisor engine accepted a 64 MiB service: %v", err)
	}
	runc, _ := isolation.NewPolicy(isolation.ClassRuncUserNS)
	if _, err := (&containerdEngine{isolation: runc}).specOpts(svc, nil, "", ""); err != nil {
		t.Fatalf("runc-userns engine rejected a 64 MiB service: %v", err)
	}
}
