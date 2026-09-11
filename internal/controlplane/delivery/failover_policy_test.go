package delivery

import (
	"testing"
	"time"
)

func TestFailoverDecision(t *testing.T) {
	base := failoverSnapshot{Allocation: policyAllocation("dead", AllocationRolloutServing, 2, time.Now()), DeadAgentID: "dead", ReusableImage: true, Generation: 2}
	for _, tc := range []struct {
		name   string
		change func(*failoverSnapshot)
		action failoverAction
	}{
		{"replace", func(s *failoverSnapshot) {}, failoverReplace},
		{"different agent", func(s *failoverSnapshot) { s.DeadAgentID = "other" }, failoverIgnore},
		{"already lost", func(s *failoverSnapshot) { s.Allocation.RolloutState = AllocationRolloutLost }, failoverIgnore},
		{"withdrawal", func(s *failoverSnapshot) {
			s.Allocation.RolloutState = AllocationRolloutWithdrawing
		}, failoverFinishDrain},
		{"drain", func(s *failoverSnapshot) {
			s.Allocation.RolloutState = AllocationRolloutDraining
		}, failoverFinishDrain},
		{"managed", func(s *failoverSnapshot) { s.ProjectKind = ProjectKindManaged }, failoverBlocked},
		{"volume", func(s *failoverSnapshot) { s.VolumeName = "data" }, failoverBlocked},
		{"no capacity", func(s *failoverSnapshot) { s.PlacementFailure = "no capacity" }, failoverBlocked},
		{"no image", func(s *failoverSnapshot) { s.ReusableImage = false }, failoverBlocked},
		{"pending change", func(s *failoverSnapshot) { s.PendingChanges = true }, failoverIgnore},
		{"pending managed change", func(s *failoverSnapshot) { s.PendingChanges = true; s.ProjectKind = ProjectKindManaged }, failoverIgnore},
		{"active target", func(s *failoverSnapshot) { s.RolloutState = rolloutStateInProgress }, failoverAdvance},
		{"active predecessor", func(s *failoverSnapshot) {
			s.RolloutState = rolloutStateInProgress
			s.Generation = 3
		}, failoverIgnore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.change(&s)
			d := decideFailover(s)
			if d.Action != tc.action || (d.Action == failoverBlocked && d.Message == "") {
				t.Fatalf("decision: %+v", d)
			}
		})
	}
}
