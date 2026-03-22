package controlplane

import (
	"context"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func TestSendLatestDesiredStateDrainsNewerRevisions(t *testing.T) {
	t.Parallel()

	states := []*agentv1.DesiredNodeState{
		{Revision: 2},
		{
			Revision: 3,
			Services: []*agentv1.DesiredService{{
				AllocationId:             "alloc-1",
				ServiceId:                "svc-1",
				DesiredSpecRevision:      1,
				DesiredRolloutGeneration: 2,
			}},
		},
		{Revision: 3},
	}
	var loads int
	var sent []int64

	lastRevision, err := sendLatestDesiredState(
		context.Background(),
		"agent-1",
		1,
		func() (*agentv1.DesiredNodeState, error) {
			if loads >= len(states) {
				return states[len(states)-1], nil
			}
			state := states[loads]
			loads++
			return state, nil
		},
		func(state *agentv1.DesiredNodeState) error {
			sent = append(sent, state.GetRevision())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("sendLatestDesiredState: %v", err)
	}
	if lastRevision != 3 {
		t.Fatalf("expected last revision 3, got %d", lastRevision)
	}
	if len(sent) != 2 || sent[0] != 2 || sent[1] != 3 {
		t.Fatalf("expected revisions [2 3], got %v", sent)
	}
}
