package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type nodeLossReplacementResult struct {
	Replaced bool
	Blocked  bool
	Changed  bool
	Bumped   bool
}

func (d *Delivery) failoverServicesFromAgent(ctx context.Context, agentID string, cutoff time.Time) ([]string, []string, error) {
	result, err := d.failoverAgent(ctx, agentID, cutoff, d.live.currentTime())
	return result.NotifyAgentIDs, result.EnvironmentIDs, err
}

func (d *Delivery) failoverAgent(ctx context.Context, agentID string, cutoff, now time.Time) (ServiceFailoverResult, error) {
	var result ServiceFailoverResult
	changedEnvironments := make(map[string]struct{})
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if d.live == nil || !d.live.Serving() {
			return ErrNotLiveOwner
		}
		agent, ok := d.live.Agent(agentID)
		if !ok || agent.LifecycleState != AgentStateUnavailable || agent.StateBeforeUnavailable != AgentStateActive || agent.LastSeenAt.After(cutoff) {
			return nil
		}
		allocations, err := listAllocationsForFailover(ctx, d.store, tx, agentID)
		if err != nil {
			return err
		}
		for _, allocation := range allocations {
			replacement, err := d.replaceLostNodeAllocationTx(ctx, tx, agentID, allocation, now)
			if err != nil {
				return err
			}
			if !replacement.Changed {
				continue
			}
			if replacement.Blocked {
				result.BlockedServiceIDs = append(result.BlockedServiceIDs, allocation.ServiceID)
			} else if replacement.Replaced {
				result.MovedServiceIDs = append(result.MovedServiceIDs, allocation.ServiceID)
			}
			result.IngressChanged = true
			changedEnvironments[allocation.EnvironmentID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return ServiceFailoverResult{}, err
	}
	if result.IngressChanged {
		result.NotifyAgentIDs = d.live.AgentIDs()
		for environmentID := range changedEnvironments {
			result.EnvironmentIDs = append(result.EnvironmentIDs, environmentID)
		}
		sort.Strings(result.EnvironmentIDs)
	}
	return result, nil
}

func (d *Delivery) replaceLostNodeAllocationTx(ctx context.Context, tx *sql.Tx, deadAgentID string, allocation AllocationRecord, now time.Time) (nodeLossReplacementResult, error) {
	s := d.store
	var result nodeLossReplacementResult
	switch lostAllocationDisposition(allocation, deadAgentID) {
	case failoverIgnore:
		return result, nil
	case failoverFinishDrain:
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{
			Kind: DecisionMarkLost, AllocationID: allocation.ID, Message: "node lost while draining; allocation will be removed",
		})); err != nil {
			return result, err
		}
		return nodeLossReplacementResult{Changed: true, Replaced: true}, nil
	}

	service, err := s.serviceByIDInternalQuerier(ctx, tx, allocation.ServiceID)
	if err != nil {
		return result, err
	}
	pendingChanges, err := s.serviceHasUnappliedChangesQuerier(ctx, tx, service)
	if err != nil {
		return result, err
	}
	if pendingChanges {
		return result, nil
	}
	project, err := s.projectByIDInternalQuerier(ctx, tx, service.ProjectID)
	if err != nil {
		return result, err
	}

	snapshot := failoverSnapshot{
		Allocation: allocation, DeadAgentID: deadAgentID,
		ProjectKind: project.Kind, VolumeName: ServiceVolumeName(service.Spec),
		Generation: service.RolloutGeneration,
	}
	// Pinned workloads do not need a placement query.
	if failoverPinnedMessage(snapshot.ProjectKind, snapshot.VolumeName) == "" {
		occupied := map[string]struct{}{deadAgentID: {}}
		rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM allocations WHERE service_id = $1 AND id <> $2 AND rollout_state <> $3`, allocation.ServiceID, allocation.ID, AllocationRolloutLost)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return result, err
			}
			occupied[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return result, err
		}
		if _, err := d.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied); errors.Is(err, ErrNoPlacementAvailable) {
			snapshot.PlacementFailure = strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrNoPlacementAvailable.Error()+": "))
			if snapshot.PlacementFailure == "" {
				snapshot.PlacementFailure = "no healthy non-reserved agent has sufficient capacity"
			}
		} else if err != nil {
			return result, err
		}
	}
	current, ok, err := s.currentDeploymentTx(ctx, tx, allocation.ServiceID)
	if err != nil {
		return result, err
	}
	snapshot.ReusableImage = ok && current.ResolvedSpec != nil && strings.TrimSpace(current.ImageDigest) != ""
	rollout, hasRollout, err := loadCurrentRolloutTx(ctx, tx, service)
	if err != nil {
		return result, err
	}
	if hasRollout {
		snapshot.RolloutState = rollout.State
	}
	decision := decideFailover(snapshot)
	if decision.Action == failoverIgnore {
		return result, nil
	}
	if decision.Action == failoverBlocked {
		state := allocationFailoverState{
			phase: allocation.Phase, message: allocation.Message,
			healthyIPv4Ports: allocation.HealthyIPv4Ports, healthyIPv6Ports: allocation.HealthyIPv6Ports, healthy: allocation.Healthy,
		}
		changed, err := d.store.markAllocationUnavailableForFailoverTx(ctx, tx, allocation.ID, state, decision.Message, now)
		if err != nil {
			return result, err
		}
		if snapshot.PlacementFailure != "" {
			if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, decision.Message, now); err != nil {
				return result, err
			}
		}
		return nodeLossReplacementResult{Blocked: true, Changed: changed}, nil
	}

	lostMessage := fmt.Sprintf("node lost; replacement scheduled from expired agent %s", deadAgentID)

	if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{
		Kind: DecisionMarkLost, AllocationID: allocation.ID, Message: lostMessage,
	})); err != nil {
		return result, err
	}
	if decision.Action == failoverAdvance {
		if _, err := d.advanceRolloutTx(ctx, tx, service.ID, now); err != nil {
			return result, err
		}
		result.Replaced = true
		result.Changed = true
		return result, nil
	}

	detail := fmt.Sprintf("Node loss replacing allocation %s from %s", allocation.ID, deadAgentID)
	if _, err := d.copyDeploymentRolloutTargetTx(ctx, tx, service, current, "", reasonFailoverRescheduled, detail, allocation.ID); err != nil {
		return result, err
	}
	result.Replaced = true
	result.Changed = true
	result.Bumped = true
	return result, nil
}
