package delivery

import (
	"fmt"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// rolloutSnapshot is read under the service lock. Decisions never mutate it.
type rolloutSnapshot struct {
	Rollout     rolloutRecord
	Allocations []AllocationRecord
	Removing    bool
}

type rolloutWithdrawal struct{ AllocationID, Message string }

type rolloutPlan struct {
	Result                    rolloutAdvanceResult
	Remove, Promote           []AllocationRecord
	Withdraw                  []rolloutWithdrawal
	Failure                   string
	FailureTargets            []AllocationRecord
	Complete, CompleteRemoval bool
	PlacementSlots            int
	Allocations               []AllocationRecord
	Continue                  bool
	PlacementMessage          string
}

func decideRollout(snapshot rolloutSnapshot, now time.Time) rolloutPlan {
	plan := rolloutPlan{}
	rollout := snapshot.Rollout
	allocs := append([]AllocationRecord(nil), snapshot.Allocations...)
	kept := make([]AllocationRecord, 0, len(allocs))
	for _, alloc := range allocs {
		drained := alloc.RolloutState == AllocationRolloutDraining &&
			(alloc.Phase == "Drained" || (alloc.DrainDeadline.Valid && !now.Before(alloc.DrainDeadline.Time)))
		if !drained {
			kept = append(kept, alloc)
			continue
		}
		plan.Remove = append(plan.Remove, alloc)
		plan.Result.Changed = true
		plan.Result.AgentIDs = append(plan.Result.AgentIDs, alloc.AgentID)
	}
	allocs = kept

	for _, alloc := range allocs {
		if alloc.RolloutState == AllocationRolloutWithdrawing {
			plan.Result.NeedsIngressConvergence = true
			plan.Result.IngressChanged = true
			return plan
		}
	}
	if snapshot.Removing {
		if len(allocs) == 0 {
			plan.CompleteRemoval = true
			plan.Result.Changed = true
			plan.Result.IngressChanged = true
		}
		return plan
	}
	if rollout.State != rolloutStateInProgress {
		return plan
	}

	target, predecessors, _ := splitRolloutAllocations(allocs, rollout)
	desiredTargetCount := rolloutTargetReplicaCount(rollout)

	for _, alloc := range target {
		if alloc.RolloutState != AllocationRolloutStarting || AllocationReady(alloc) {
			continue
		}
		failure := rolloutAllocationFailure(alloc, now, rollout.Strategy)
		if failure == "" {
			continue
		}
		plan.Failure = failure
		plan.FailureTargets = rolloutFailureCleanup(target)
		plan.Result.Changed = true
		plan.Result.AgentIDs = appendAllocationAgentIDs(plan.Result.AgentIDs, target...)
		return plan
	}

	readyStarting := filterAllocations(target, func(a AllocationRecord) bool {
		return a.RolloutState == AllocationRolloutStarting && AllocationReady(a)
	})
	servingTarget := filterAllocations(target, func(a AllocationRecord) bool {
		return a.RolloutState == AllocationRolloutServing && AllocationReady(a)
	})
	servingOld := filterAllocations(predecessors, func(a AllocationRecord) bool {
		return a.RolloutState == AllocationRolloutServing
	})
	sortAllocationsStable(readyStarting)
	sortAllocationsStable(servingOld)

	promote := len(readyStarting)
	if len(predecessors) > 0 {
		needed := int(desiredTargetCount) - len(servingTarget)
		if needed < 0 {
			needed = 0
		}
		withoutPredecessor := needed - len(servingOld)
		if withoutPredecessor < 0 {
			withoutPredecessor = 0
		}
		capPromote := len(servingOld) + withoutPredecessor
		if promote > capPromote {
			promote = capPromote
		}
	}
	if promote > 0 {
		for index := 0; index < promote; index++ {
			newAlloc := readyStarting[index]
			plan.Promote = append(plan.Promote, newAlloc)
			for i := range allocs {
				if allocs[i].ID == newAlloc.ID {
					allocs[i].RolloutState = AllocationRolloutServing
				}
			}
			servingTarget = append(servingTarget, newAlloc)
			plan.Result.AgentIDs = append(plan.Result.AgentIDs, newAlloc.AgentID)
			if index >= len(servingOld) {
				continue
			}
			oldAlloc := servingOld[index]
			plan.Withdraw = append(plan.Withdraw, rolloutWithdrawal{oldAlloc.ID, "replacement is ready; waiting for ingress withdrawal"})
			for i := range allocs {
				if allocs[i].ID == oldAlloc.ID {
					allocs[i].RolloutState = AllocationRolloutWithdrawing
				}
			}
			plan.Result.IngressChanged = true
			plan.Result.NeedsIngressConvergence = true
		}
		plan.Result.Changed = true
	}

	target, predecessors, _ = splitRolloutAllocations(allocs, rollout)
	servingTarget = filterAllocations(target, func(a AllocationRecord) bool {
		return a.RolloutState == AllocationRolloutServing && AllocationReady(a)
	})
	if int32(len(servingTarget)) >= desiredTargetCount && len(predecessors) == 0 {
		plan.Complete = true
		plan.Result.Changed = true
		plan.Result.IngressChanged = true
		return plan
	}

	missing := int(desiredTargetCount) - len(target)
	limit := int(rollout.DesiredReplicaCount)
	if len(predecessors) > 0 {
		limit += defaultRolloutMaxSurge
	}
	plan.PlacementSlots = max(0, min(missing, limit-len(filterAllocations(allocs, allocationOccupiesRolloutSlot))))
	plan.Allocations = allocs
	plan.Continue = true
	return plan
}

func decideRolloutPlacement(snapshot rolloutSnapshot, created int, ingressPending bool, now time.Time) rolloutPlan {
	plan := rolloutPlan{Result: rolloutAdvanceResult{NeedsIngressConvergence: ingressPending}}
	rollout, allocs := snapshot.Rollout, snapshot.Allocations
	target, predecessors, unaffected := splitRolloutAllocations(allocs, rollout)
	desiredTargetCount := rolloutTargetReplicaCount(rollout)
	missing := int(desiredTargetCount) - len(target)
	servingTarget := filterAllocations(target, func(a AllocationRecord) bool { return a.RolloutState == AllocationRolloutServing && AllocationReady(a) })
	var servingOld []AllocationRecord
	if !plan.Result.NeedsIngressConvergence {
		servingOld = filterAllocations(predecessors, func(a AllocationRecord) bool {
			return a.RolloutState == AllocationRolloutServing
		})
		available := len(servingTarget)
		for _, alloc := range servingOld {
			if AllocationReady(alloc) {
				available++
			}
		}
		for _, alloc := range unaffected {
			if alloc.RolloutState == AllocationRolloutServing && AllocationReady(alloc) {
				available++
			}
		}
		minimumAvailable := int(rollout.DesiredReplicaCount) - defaultRolloutMaxUnavailable
		if minimumAvailable < 0 {
			minimumAvailable = 0
		}
		limit := int(rollout.DesiredReplicaCount)
		if len(predecessors) > 0 {
			limit += defaultRolloutMaxSurge
		}
		over := len(filterAllocations(allocs, allocationOccupiesRolloutSlot)) + created - limit
		room := missing - created
		withdraw := available - minimumAvailable
		if withdraw < over {
			withdraw = over
		}
		if room > 0 && withdraw > room {
			withdraw = room
		}
		if withdraw > len(servingOld) {
			withdraw = len(servingOld)
		}
		if withdraw < 0 {
			withdraw = 0
		}
		sortAllocationsStable(servingOld)
		for index := 0; index < withdraw; index++ {
			message := "strategy allows temporary unavailability; waiting for ingress withdrawal"
			if over > 0 {
				message = "surplus predecessor; waiting for ingress withdrawal"
			}
			plan.Withdraw = append(plan.Withdraw, rolloutWithdrawal{servingOld[index].ID, message})
			plan.Result.Changed = true
			plan.Result.IngressChanged = true
			plan.Result.NeedsIngressConvergence = true
		}
	}

	placement := ""
	if missing > 0 && created < missing && !plan.Result.NeedsIngressConvergence {
		placement = pendingPlacementMessage(len(target)+created, int(desiredTargetCount),
			"no eligible agent has spare capacity for a replacement")
	}
	plan.PlacementMessage = placement

	if missing > 0 && created == 0 && !plan.Result.NeedsIngressConvergence &&
		now.Sub(rollout.ProgressAt) >= defaultRolloutSchedulingWait {
		reason := fmt.Sprintf("could not schedule a replacement within %s: no eligible agent has spare capacity for a replacement",
			defaultRolloutSchedulingWait)
		plan.Failure = reason
		plan.FailureTargets = rolloutFailureCleanup(target)
		plan.Result.Changed = true
		plan.Result.AgentIDs = appendAllocationAgentIDs(plan.Result.AgentIDs, target...)
	}

	return plan
}

func rolloutAllocationFailure(alloc AllocationRecord, now time.Time, strategy *platformv1.RollingStrategy) string {
	switch alloc.Phase {
	case "Error", "Failed", "Unhealthy", "Stopped", "CrashLoop":
		return fmt.Sprintf("replacement allocation %s failed readiness: %s", alloc.ID, FirstNonEmpty(alloc.Message, alloc.Phase))
	}
	if alloc.Restart.GetCrashLoop() || alloc.Phase == "CrashLoop" {
		return fmt.Sprintf("replacement allocation %s entered a crash loop: %s", alloc.ID, FirstNonEmpty(alloc.Message, "restart budget exhausted"))
	}
	deadline := alloc.CreatedAt.Add(time.Duration(strategy.GetHealthcheckTimeoutSeconds()) * time.Second)
	if !now.Before(deadline) {
		return fmt.Sprintf("replacement allocation %s did not become ready within %s: %s", alloc.ID,
			time.Duration(strategy.GetHealthcheckTimeoutSeconds())*time.Second,
			FirstNonEmpty(alloc.Message, "readiness check did not pass"))
	}
	return ""
}

func rolloutTargetReplicaCount(rollout rolloutRecord) int32 {
	if rollout.TargetAllocationID != "" {
		return 1
	}
	return rollout.DesiredReplicaCount
}

func filterAllocations(input []AllocationRecord, keep func(AllocationRecord) bool) []AllocationRecord {
	out := make([]AllocationRecord, 0, len(input))
	for _, alloc := range input {
		if keep(alloc) {
			out = append(out, alloc)
		}
	}
	return out
}

func sortAllocationsStable(input []AllocationRecord) {
	sort.Slice(input, func(i, j int) bool {
		if !input[i].CreatedAt.Equal(input[j].CreatedAt) {
			return input[i].CreatedAt.Before(input[j].CreatedAt)
		}
		return input[i].ID < input[j].ID
	})
}

func appendAllocationAgentIDs(ids []string, allocations ...AllocationRecord) []string {
	for _, alloc := range allocations {
		if strings.TrimSpace(alloc.AgentID) != "" {
			ids = append(ids, alloc.AgentID)
		}
	}
	return ids
}

func allocationOccupiesRolloutSlot(alloc AllocationRecord) bool {
	switch alloc.RolloutState {
	case AllocationRolloutStarting, AllocationRolloutServing, AllocationRolloutWithdrawing:
		return true
	default:
		return false
	}
}

func splitRolloutAllocations(allocs []AllocationRecord, rollout rolloutRecord) (target, predecessors, unaffected []AllocationRecord) {
	for _, alloc := range allocs {
		if alloc.RolloutState == AllocationRolloutLost {
			continue
		}
		if alloc.DesiredRolloutGeneration == rollout.Generation {
			target = append(target, alloc)
			continue
		}
		if alloc.DesiredRolloutGeneration < rollout.Generation {
			if rollout.TargetAllocationID == "" || alloc.ID == rollout.TargetAllocationID {
				predecessors = append(predecessors, alloc)
			} else {
				unaffected = append(unaffected, alloc)
			}
		}
	}
	return target, predecessors, unaffected
}

// Serving replacements survive partial rollout failure; only never-routed
// starting allocations may bypass ingress withdrawal for cleanup.
func rolloutFailureCleanup(target []AllocationRecord) []AllocationRecord {
	return filterAllocations(target, func(a AllocationRecord) bool { return a.RolloutState == AllocationRolloutStarting })
}
