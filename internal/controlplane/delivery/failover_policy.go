package delivery

import "fmt"

type failoverAction uint8

const (
	failoverIgnore failoverAction = iota
	failoverFinishDrain
	failoverBlocked
	failoverAdvance
	failoverReplace
)

type failoverSnapshot struct {
	Allocation       AllocationRecord
	DeadAgentID      string
	ProjectKind      ProjectKind
	VolumeName       string
	PlacementFailure string
	ReusableImage    bool
	RolloutState     string
	Generation       int64
}

type failoverDecision struct {
	Action  failoverAction
	Message string
}

func lostAllocationDisposition(allocation AllocationRecord, deadAgentID string) failoverAction {
	if allocation.AgentID != deadAgentID || allocation.RolloutState == AllocationRolloutLost {
		return failoverIgnore
	}
	if allocation.RolloutState == AllocationRolloutDraining || allocation.RolloutState == AllocationRolloutWithdrawing {
		return failoverFinishDrain
	}
	return failoverReplace
}

func failoverPinnedMessage(kind ProjectKind, volumeName string) string {
	if kind == ProjectKindManaged {
		return "agent unhealthy; managed/trusted workload remains pinned to its trusted agent"
	}
	if volumeName != "" {
		return fmt.Sprintf("agent unhealthy; service remains pinned because node-bound volume %q requires replicated storage before failover", volumeName)
	}
	return ""
}

func failoverPlacementMessage(detail string) string {
	if detail == "" {
		detail = "no healthy non-reserved agent has sufficient capacity"
	}
	return "agent unhealthy; automatic failover blocked because " + detail
}

func decideFailover(snapshot failoverSnapshot) failoverDecision {
	if action := lostAllocationDisposition(snapshot.Allocation, snapshot.DeadAgentID); action != failoverReplace {
		return failoverDecision{Action: action}
	}
	if message := failoverPinnedMessage(snapshot.ProjectKind, snapshot.VolumeName); message != "" {
		return failoverDecision{Action: failoverBlocked, Message: message}
	}
	if snapshot.PlacementFailure != "" {
		return failoverDecision{Action: failoverBlocked, Message: failoverPlacementMessage(snapshot.PlacementFailure)}
	}
	if !snapshot.ReusableImage {
		return failoverDecision{Action: failoverBlocked, Message: "agent unhealthy; service has no reusable image snapshot for a rolling replacement"}
	}
	if snapshot.RolloutState == rolloutStateInProgress || snapshot.RolloutState == rolloutStatePendingBuild {
		if snapshot.Allocation.DesiredRolloutGeneration < snapshot.Generation {
			return failoverDecision{Action: failoverIgnore}
		}
		return failoverDecision{Action: failoverAdvance}
	}
	return failoverDecision{Action: failoverReplace}
}
