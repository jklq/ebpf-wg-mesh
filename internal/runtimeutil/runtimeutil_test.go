package runtimeutil

import (
	"slices"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestRuntimePortNumbersDedupesAndSkipsInvalid(t *testing.T) {
	t.Parallel()

	runtime := &platformv1.ServiceRuntime{
		Ports: []*platformv1.ServiceRuntimePort{
			{Port: 8080},
			{Port: 8080},
			{Port: 0},
			{Port: 70000},
			{Port: 9090},
		},
	}
	if got, want := RuntimePortNumbers(runtime), []int32{8080, 9090}; !slices.Equal(got, want) {
		t.Fatalf("RuntimePortNumbers() = %v, want %v", got, want)
	}
	if RuntimePortNumbers(nil) != nil {
		t.Fatal("RuntimePortNumbers(nil) should be nil")
	}
}

func TestReadinessCheckPortPrefersExplicitThenPrimary(t *testing.T) {
	t.Parallel()

	runtime := &platformv1.ServiceRuntime{
		Ports: []*platformv1.ServiceRuntimePort{
			{Port: 8080},
			{Port: 9090, Primary: true},
		},
	}
	if got := ReadinessCheckPort(runtime, &platformv1.HealthCheck{Port: 1234}); got != 1234 {
		t.Fatalf("explicit check port = %d, want 1234", got)
	}
	if got := ReadinessCheckPort(runtime, &platformv1.HealthCheck{}); got != 9090 {
		t.Fatalf("primary port = %d, want 9090", got)
	}
	if got := ReadinessCheckPort(&platformv1.ServiceRuntime{}, &platformv1.HealthCheck{}); got != 0 {
		t.Fatalf("empty runtime port = %d, want 0", got)
	}
}

func TestReadinessPortsFallsBackToHealthCheckPort(t *testing.T) {
	t.Parallel()

	withPorts := &agentv1.DesiredService{
		Spec: &platformv1.ResolvedServiceSpec{
			Runtime: &platformv1.ServiceRuntime{
				Ports: []*platformv1.ServiceRuntimePort{{Port: 8080}},
			},
		},
	}
	if got := ReadinessPorts(withPorts); !slices.Equal(got, []int32{8080}) {
		t.Fatalf("ReadinessPorts() = %v, want [8080]", got)
	}

	healthOnly := &agentv1.DesiredService{
		Spec: &platformv1.ResolvedServiceSpec{
			Runtime: &platformv1.ServiceRuntime{
				HealthCheck: &platformv1.HealthCheck{Port: 9090},
			},
		},
	}
	if got := ReadinessPorts(healthOnly); !slices.Equal(got, []int32{9090}) {
		t.Fatalf("ReadinessPorts() = %v, want [9090]", got)
	}
}

func TestIndexDesiredHelpers(t *testing.T) {
	t.Parallel()

	volumes := []*agentv1.DesiredVolume{{VolumeId: "vol-1"}}
	if got := IndexDesiredVolumes(volumes)["vol-1"]; got != volumes[0] {
		t.Fatal("IndexDesiredVolumes did not index by volume ID")
	}
	services := []*agentv1.DesiredService{{AllocationId: "alloc-1"}}
	if got := IndexDesiredServices(services)["alloc-1"]; got != services[0] {
		t.Fatal("IndexDesiredServices did not index by allocation ID")
	}
}
