package reconciliation

import (
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPartitionedAgentRejectsFormerAuthorityWithoutSeeingSuccessor(t *testing.T) {
	issued := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	deadline := issued.Add(GrantLifetime)
	command := &agentv1.DesiredNodeState{SessionId: "former", AuthorityEpoch: 7, AuthorityNotAfter: timestamppb.New(deadline)}
	for _, skew := range []time.Duration{-MaxClockSkew, 0, MaxClockSkew} {
		if err := ValidateCommand(command, "former", issued.Add(skew)); err != nil {
			t.Fatal(err)
		}
		// The isolated agent never learns epoch 8. Expiry must suffice, including
		// the slowest permitted clock and a process paused across takeover.
		if err := ValidateCommand(command, "former", deadline.Add(skew)); err == nil {
			t.Fatalf("accepted former authority with clock skew %s", skew)
		}
	}
	if CanTakeOver(deadline.Add(-time.Nanosecond), deadline) {
		t.Fatal("early takeover")
	}
	if !CanTakeOver(deadline, deadline) {
		t.Fatal("takeover blocked after expiry")
	}
	if err := ValidateCommand(command, "replacement", issued); err == nil {
		t.Fatal("delayed message crossed sessions")
	}
	command.AuthorityNotAfter = timestamppb.New(deadline.Add(time.Hour))
	if err := ValidateCommand(command, "former", issued); err == nil {
		t.Fatal("unbounded grant accepted")
	}
}

func TestHashObservationOverlay(t *testing.T) {
	t.Parallel()
	overlayService := func(id, hostname, ipv4 string) *agentv1.DesiredService {
		return &agentv1.DesiredService{
			AllocationId: id,
			InternalHosts: []*agentv1.InternalHost{
				{Hostname: hostname, Ipv4: ipv4},
			},
		}
	}
	base := HashObservationOverlay([]*agentv1.DesiredService{
		overlayService("alloc-a", "web-a.mesh.internal", "10.0.0.1"),
		overlayService("alloc-b", "web-b.mesh.internal", "10.0.0.2"),
	})
	// Desired configuration is a set: service and host order is not semantic.
	reordered := HashObservationOverlay([]*agentv1.DesiredService{
		{
			AllocationId: "alloc-b",
			InternalHosts: []*agentv1.InternalHost{
				{Hostname: "web-b.mesh.internal", Ipv6: "fd00::2"},
				{Hostname: "web-b.mesh.internal", Ipv4: "10.0.0.2"},
			},
		},
		overlayService("alloc-a", "web-a.mesh.internal", "10.0.0.1"),
	})
	if base == reordered {
		t.Fatal("added internal host did not change the overlay version")
	}
	same := HashObservationOverlay([]*agentv1.DesiredService{
		{
			AllocationId:  "alloc-b",
			InternalHosts: []*agentv1.InternalHost{{Hostname: "web-b.mesh.internal", Ipv4: "10.0.0.2"}},
		},
		overlayService("alloc-a", "web-a.mesh.internal", "10.0.0.1"),
	})
	if base != same {
		t.Fatal("wire order changed the overlay version")
	}
	// Health-gated withdrawal of a host is drift.
	if base == HashObservationOverlay([]*agentv1.DesiredService{
		overlayService("alloc-a", "web-a.mesh.internal", "10.0.0.1"),
	}) {
		t.Fatal("removed service did not change the overlay version")
	}
	// Restart observations are part of the overlay.
	withRestart := HashObservationOverlay([]*agentv1.DesiredService{
		overlayService("alloc-a", "web-a.mesh.internal", "10.0.0.1"),
		{
			AllocationId:       "alloc-b",
			InternalHosts:      []*agentv1.InternalHost{{Hostname: "web-b.mesh.internal", Ipv4: "10.0.0.2"}},
			RestartObservation: &platformv1.RestartObservation{RestartCount: 3},
		},
	})
	if base == withRestart {
		t.Fatal("restart observation did not change the overlay version")
	}
	if HashObservationOverlay(nil) != HashObservationOverlay(nil) {
		t.Fatal("empty overlay is not deterministic")
	}
}
