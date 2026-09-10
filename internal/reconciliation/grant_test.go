package reconciliation

import (
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
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
