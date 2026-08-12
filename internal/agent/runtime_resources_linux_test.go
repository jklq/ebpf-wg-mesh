//go:build linux

package agent

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
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
