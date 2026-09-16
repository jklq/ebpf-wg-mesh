package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"time"
)

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
)

type SchedulingDecision struct {
	Kind         SchedulingDecisionKind
	Allocation   AllocationAssignment
	AllocationID string
	State        allocationAssignmentState
	Message      string
}

type SchedulingPlan struct {
	DecidedAt time.Time
	Decisions []SchedulingDecision
}

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
		default:
			return fmt.Errorf("scheduler decision %d has unknown kind %q", i, decision.Kind)
		}
	}
	return nil
}

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
			journal.RecordAssignment(ctx, assignment.ID)
		case DecisionReserveAddress:
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
				SET allocation_ipv4 = $2, updated_at = $3 WHERE id = $1`,
				decision.AllocationID, decision.Allocation.IPv4, plan.DecidedAt); err != nil {
				return err
			}
			journal.RecordAssignment(ctx, decision.AllocationID)
		}
	}
	return nil
}

func allocationMutationPlan(now time.Time, decisions ...SchedulingDecision) SchedulingPlan {
	return SchedulingPlan{DecidedAt: now.UTC(), Decisions: decisions}
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
