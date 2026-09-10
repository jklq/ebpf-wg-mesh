package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SchedulerCause describes why the scheduler was asked to evaluate state. It
// is input to policy, not an instruction to mutate storage directly.
type SchedulerCause string

const (
	SchedulerDesiredChange      SchedulerCause = "desired_change"
	SchedulerAgentRegistration  SchedulerCause = "agent_registration"
	SchedulerAllocationObserved SchedulerCause = "allocation_observed"
	SchedulerAgentLost          SchedulerCause = "agent_lost"
	SchedulerOperatorAction     SchedulerCause = "operator_action"
	SchedulerCapacityChanged    SchedulerCause = "capacity_changed"
	SchedulerDeadline           SchedulerCause = "deadline"
)

// SchedulerEvaluation is the consistent, immutable view used for one policy
// decision. DecidedAt is supplied by the caller (normally database time), so
// replay never consults a process clock.
type SchedulerEvaluation struct {
	Cause       SchedulerCause
	DecidedAt   time.Time
	Services    []ServiceRecord
	Agents      []AgentRecord
	Allocations []AllocationRecord
	// Requested contains already-resolved actions from release, rollout,
	// failover, report, or fleet policy. The evaluator records those concrete
	// values; it never repeats placement while applying a plan.
	Requested []SchedulingDecision
}

// EvaluateScheduler is storage independent. Deadline policy is evaluated from
// the supplied snapshot and all requested decisions are copied into the plan.
// Calling it twice with the same input produces the same plan.
func EvaluateScheduler(input SchedulerEvaluation) SchedulingPlan {
	plan := SchedulingPlan{
		DecidedAt: input.DecidedAt.UTC(),
		Decisions: append([]SchedulingDecision(nil), input.Requested...),
	}
	alreadyCompleted := make(map[string]struct{}, len(plan.Decisions))
	var nextEvaluation time.Time
	for _, decision := range plan.Decisions {
		if decision.Kind == DecisionCompleteDrain {
			alreadyCompleted[decision.AllocationID] = struct{}{}
		}
	}
	for _, allocation := range input.Allocations {
		if allocation.RolloutState != AllocationRolloutDraining || !allocation.DrainDeadline.Valid {
			continue
		}
		if input.DecidedAt.Before(allocation.DrainDeadline.Time) {
			if nextEvaluation.IsZero() || allocation.DrainDeadline.Time.Before(nextEvaluation) {
				nextEvaluation = allocation.DrainDeadline.Time
			}
			continue
		}
		if _, exists := alreadyCompleted[allocation.ID]; exists {
			continue
		}
		plan.Decisions = append(plan.Decisions, SchedulingDecision{Kind: DecisionCompleteDrain, AllocationID: allocation.ID})
		alreadyCompleted[allocation.ID] = struct{}{}
	}
	if !nextEvaluation.IsZero() {
		plan.Decisions = append(plan.Decisions, SchedulingDecision{Kind: DecisionEvaluateAt, EvaluateAt: nextEvaluation.UTC()})
	}
	return plan
}

type SchedulingDecisionKind string

const (
	DecisionCreateAllocation SchedulingDecisionKind = "create_allocation"
	DecisionChangeIntent     SchedulingDecisionKind = "change_allocation_intent"
	DecisionSetMessage       SchedulingDecisionKind = "set_allocation_message"
	DecisionRetarget         SchedulingDecisionKind = "retarget_allocation"
	DecisionReserveAddress   SchedulingDecisionKind = "reserve_allocation_address"
	DecisionBeginDrain       SchedulingDecisionKind = "begin_drain"
	DecisionCompleteDrain    SchedulingDecisionKind = "complete_drain"
	DecisionMarkLost         SchedulingDecisionKind = "mark_allocation_lost"
	DecisionAdvanceDeploy    SchedulingDecisionKind = "advance_deployment"
	DecisionFailDeploy       SchedulingDecisionKind = "fail_deployment"
	DecisionEvaluateAt       SchedulingDecisionKind = "evaluate_at"
)

// SchedulingDecision is a concrete, replayable decision. IDs, addresses and
// timestamps are values in the decision; applying it does not rerun placement.
type SchedulingDecision struct {
	Kind         SchedulingDecisionKind
	Allocation   AllocationAssignment
	AllocationID string
	State        allocationAssignmentState
	Message      string
	DeploymentID string
	EvaluateAt   time.Time
}

type SchedulingPlan struct {
	DecidedAt time.Time
	Decisions []SchedulingDecision
}

// validate makes malformed plans fail before their first desired-state write.
func (p SchedulingPlan) validate() error {
	if p.DecidedAt.IsZero() {
		return fmt.Errorf("scheduler plan has no decision timestamp")
	}
	for i, decision := range p.Decisions {
		switch decision.Kind {
		case DecisionCreateAllocation:
			if decision.Allocation.ID == "" || decision.Allocation.AgentID == "" || decision.Allocation.IPv4 == "" || decision.Allocation.IPv6 == "" {
				return fmt.Errorf("scheduler decision %d has an incomplete allocation reservation", i)
			}
		case DecisionChangeIntent, DecisionBeginDrain:
			if decision.State.AllocationID == "" {
				return fmt.Errorf("scheduler decision %d has no allocation", i)
			}
		case DecisionCompleteDrain, DecisionMarkLost, DecisionSetMessage:
			if decision.AllocationID == "" {
				return fmt.Errorf("scheduler decision %d has no allocation", i)
			}
		case DecisionReserveAddress:
			if decision.AllocationID == "" || decision.Allocation.IPv4 == "" {
				return fmt.Errorf("scheduler decision %d has an incomplete address reservation", i)
			}
		case DecisionRetarget:
			if decision.Allocation.ID == "" || decision.Allocation.DeploymentID == "" {
				return fmt.Errorf("scheduler decision %d has an incomplete retarget", i)
			}
		case DecisionAdvanceDeploy, DecisionFailDeploy:
			if decision.DeploymentID == "" {
				return fmt.Errorf("scheduler decision %d has no deployment", i)
			}
		case DecisionEvaluateAt:
			if decision.EvaluateAt.IsZero() {
				return fmt.Errorf("scheduler decision %d has no evaluation deadline", i)
			}
		default:
			return fmt.Errorf("scheduler decision %d has unknown kind %q", i, decision.Kind)
		}
	}
	return nil
}

// applySchedulingPlanTx is the sole write gateway for scheduler-owned
// allocation assignments. A recorded plan can be applied verbatim; placement,
// clocks and ID generation are deliberately absent from this method.
func (d *Delivery) applySchedulingPlanTx(ctx context.Context, tx *sql.Tx, plan SchedulingPlan) error {
	if err := plan.validate(); err != nil {
		return err
	}
	for _, decision := range plan.Decisions {
		switch decision.Kind {
		case DecisionCreateAllocation:
			if err := d.store.insertAllocationAssignmentTx(ctx, tx, decision.Allocation); err != nil {
				return err
			}
		case DecisionChangeIntent, DecisionBeginDrain:
			if err := d.store.setAllocationStateTx(ctx, tx, decision.State); err != nil {
				return err
			}
		case DecisionCompleteDrain:
			if err := d.store.deleteAllocationAssignmentTx(ctx, tx, decision.AllocationID); err != nil {
				return err
			}
		case DecisionMarkLost:
			if err := d.store.markAssignmentLostTx(ctx, tx, decision.AllocationID, decision.Message, plan.DecidedAt); err != nil {
				return err
			}
		case DecisionSetMessage:
			if err := d.store.setAssignmentMessageTx(ctx, tx, decision.AllocationID, decision.Message, plan.DecidedAt); err != nil {
				return err
			}
		case DecisionRetarget:
			assignment := decision.Allocation
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
				SET deployment_id = $2, desired_spec_revision = $3, desired_rollout_generation = $4,
				    intent = $5, intent_message = '', updated_at = $6 WHERE id = $1`,
				assignment.ID, assignment.DeploymentID, assignment.SpecRevision, assignment.RolloutGeneration,
				assignment.Intent, plan.DecidedAt); err != nil {
				return err
			}
		case DecisionReserveAddress:
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
				SET allocation_ipv4 = $2, updated_at = $3 WHERE id = $1`,
				decision.AllocationID, decision.Allocation.IPv4, plan.DecidedAt); err != nil {
				return err
			}
		case DecisionAdvanceDeploy, DecisionFailDeploy, DecisionEvaluateAt:
			// Deployment transitions and wake scheduling are consumed by the
			// orchestration layer after allocation decisions are committed.
		}
	}
	return nil
}

func allocationMutationPlan(now time.Time, decisions ...SchedulingDecision) SchedulingPlan {
	return EvaluateScheduler(SchedulerEvaluation{
		Cause: SchedulerDesiredChange, DecidedAt: now.UTC(), Requested: decisions,
	})
}

func (d *Delivery) deleteStartingAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID string, generation *int64, now time.Time) error {
	query := `SELECT id FROM allocation_assignments WHERE service_id = $1 AND rollout_state = $2`
	args := []any{serviceID, AllocationRolloutStarting}
	if generation != nil {
		query += ` AND desired_rollout_generation = $3`
		args = append(args, *generation)
	}
	query += ` ORDER BY id FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	var decisions []SchedulingDecision
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		decisions = append(decisions, SchedulingDecision{Kind: DecisionCompleteDrain, AllocationID: id})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, decisions...))
}

func (d *Delivery) withdrawServiceAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND rollout_state NOT IN ($2, $3, $4) ORDER BY id FOR UPDATE`,
		serviceID, AllocationRolloutWithdrawing, AllocationRolloutDraining, AllocationRolloutLost)
	if err != nil {
		return 0, err
	}
	var decisions []SchedulingDecision
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		decisions = append(decisions, SchedulingDecision{Kind: DecisionChangeIntent, State: allocationAssignmentState{
			AllocationID: id, RolloutState: AllocationRolloutWithdrawing, Intent: allocationIntentRun,
			IntentMessage: message, UpdatedAt: now,
		}})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return int64(len(decisions)), d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, decisions...))
}

func (d *Delivery) retargetAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, agentID, deploymentID string, specRevision, generation int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND agent_id = $2 AND rollout_state IN ($3, $4) ORDER BY id FOR UPDATE`,
		serviceID, agentID, AllocationRolloutStarting, AllocationRolloutServing)
	if err != nil {
		return err
	}
	var decisions []SchedulingDecision
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		decisions = append(decisions, SchedulingDecision{Kind: DecisionRetarget, Allocation: AllocationAssignment{
			ID: id, DeploymentID: deploymentID, SpecRevision: specRevision, RolloutGeneration: generation, Intent: allocationIntentRun,
		}})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, decisions...))
}
