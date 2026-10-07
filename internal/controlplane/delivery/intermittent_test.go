package delivery

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestIntermittentCapacityAcceptsStatelessAndRejectsDurableWorkloads(t *testing.T) {
	candidate := testCandidate("home", "oslo", "home", 0)
	candidate.HostType = config.HostIntermittent
	candidate.RuntimeCapabilities = []string{"containerd", "wireguard", "ebpf-policy"}
	spec := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 64})
	if !candidateEligible(candidate, spec) {
		t.Fatal("intermittent stateless capacity rejected")
	}
	spec.Runtime.Volume = &platformv1.ServiceVolumeMount{VolumeName: "durable"}
	if candidateEligible(candidate, spec) {
		t.Fatal("durable volume placed on intermittent capacity")
	}
}
