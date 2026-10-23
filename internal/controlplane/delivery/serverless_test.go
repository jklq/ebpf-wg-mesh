package delivery

import (
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/proto"
)

func TestReadyWakeEndpointsMirrorsIngressRule(t *testing.T) {
	ready := AllocationRecord{
		ID: "a-1", Healthy: true, RolloutState: AllocationRolloutServing,
		DesiredSpecRevision: 2, AppliedSpecRevision: 2,
		DesiredRolloutGeneration: 3, AppliedRolloutGeneration: 3,
		AllocationIPv4: "10.0.0.5", AllocationIPv6: "fd00::5",
		HealthyIPv4Ports: []int32{8080}, HealthyIPv6Ports: []int32{8080},
	}
	tests := []struct {
		name  string
		alloc AllocationRecord
		want  string // expected address, "" for none
	}{
		{name: "ipv4 preferred", alloc: ready, want: "10.0.0.5"},
		{name: "ipv6 when ipv4 port unhealthy", alloc: func() AllocationRecord {
			a := ready
			a.HealthyIPv4Ports = []int32{9090}
			return a
		}(), want: "fd00::5"},
		{name: "sleeping is never ready", alloc: func() AllocationRecord {
			a := ready
			a.RolloutState = AllocationRolloutSleeping
			return a
		}()},
		{name: "crash loop is never ready", alloc: func() AllocationRecord {
			a := ready
			a.Phase = restartpolicy.PhaseCrashLoop
			return a
		}()},
		{name: "stale generation is not ready", alloc: func() AllocationRecord {
			a := ready
			a.AppliedRolloutGeneration = 2
			return a
		}()},
		{name: "unhealthy is not ready", alloc: func() AllocationRecord {
			a := ready
			a.Healthy = false
			return a
		}()},
		{name: "wrong port is not ready", alloc: func() AllocationRecord {
			a := ready
			a.HealthyIPv4Ports = []int32{3000}
			a.HealthyIPv6Ports = []int32{3000}
			return a
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoints := readyWakeEndpoints([]AllocationRecord{tc.alloc}, 8080)
			if tc.want == "" {
				if len(endpoints) != 0 {
					t.Fatalf("expected no endpoints, got %+v", endpoints)
				}
				return
			}
			if len(endpoints) != 1 || endpoints[0].Address != tc.want || endpoints[0].Port != 8080 {
				t.Fatalf("expected one endpoint at %s:8080, got %+v", tc.want, endpoints)
			}
		})
	}
}

func TestSleepDrainGrace(t *testing.T) {
	withDraining := func(seconds int32) ServiceRecord {
		return ServiceRecord{Spec: &platformv1.ServiceSpec{RollingStrategy: &platformv1.RollingStrategy{
			DrainingSeconds: proto.Int32(seconds),
		}}}
	}
	if got := sleepDrainGrace(ServiceRecord{}); got != defaultSleepDrainGrace {
		t.Fatalf("unset draining: got %s, want default %s", got, defaultSleepDrainGrace)
	}
	if got := sleepDrainGrace(withDraining(45)); got != 45*time.Second {
		t.Fatalf("configured draining: got %s, want 45s", got)
	}
	if got := sleepDrainGrace(withDraining(3600)); got != maxSleepDrainGrace {
		t.Fatalf("oversized draining: got %s, want cap %s", got, maxSleepDrainGrace)
	}
}

func TestServerlessSpecValueAndDiscard(t *testing.T) {
	enabled := true
	spec := &platformv1.ServiceSpec{ServerlessEnabled: &enabled}
	if got := serverlessValue(spec); got != "enabled" {
		t.Fatalf("serverlessValue(enabled) = %q", got)
	}
	if got := serverlessValue(&platformv1.ServiceSpec{}); got != "disabled" {
		t.Fatalf("serverlessValue(absent) = %q", got)
	}
}
