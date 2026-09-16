package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"time"
)

type AllocationMutationKind string

const (
	MutationCreateAllocation AllocationMutationKind = "create_allocation"
	MutationChangeIntent     AllocationMutationKind = "change_allocation_intent"
	MutationSetMessage       AllocationMutationKind = "set_allocation_message"
	MutationRetarget         AllocationMutationKind = "retarget_allocation"
	MutationReserveAddress   AllocationMutationKind = "reserve_allocation_address"
	MutationBeginDrain       AllocationMutationKind = "begin_drain"
	MutationCompleteDrain    AllocationMutationKind = "complete_drain"
	MutationMarkLost         AllocationMutationKind = "mark_allocation_lost"
)

type AllocationMutation struct {
	Kind         AllocationMutationKind
	Allocation   AllocationAssignment
	AllocationID string
	State        allocationAssignmentState
	Message      string
}

func (m AllocationMutation) validate(index int) error {
	switch m.Kind {
	case MutationCreateAllocation:
		if m.Allocation.ID == "" || m.Allocation.AgentID == "" || m.Allocation.IPv4 == "" || m.Allocation.IPv6 == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete allocation reservation", index)
		}
	case MutationChangeIntent, MutationBeginDrain:
		if m.State.AllocationID == "" {
			return fmt.Errorf("allocation mutation %d has no allocation", index)
		}
	case MutationCompleteDrain, MutationMarkLost, MutationSetMessage:
		if m.AllocationID == "" {
			return fmt.Errorf("allocation mutation %d has no allocation", index)
		}
	case MutationReserveAddress:
		if m.AllocationID == "" || m.Allocation.IPv4 == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete address reservation", index)
		}
	case MutationRetarget:
		if m.Allocation.ID == "" || m.Allocation.DeploymentID == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete retarget", index)
		}
	default:
		return fmt.Errorf("allocation mutation %d has unknown kind %q", index, m.Kind)
	}
	return nil
}

func (d *Delivery) applyAllocationMutationsTx(ctx context.Context, tx *sql.Tx, now time.Time, mutations ...AllocationMutation) error {
	if now.IsZero() {
		return fmt.Errorf("allocation mutations have no timestamp")
	}
	now = now.UTC()
	for i, mutation := range mutations {
		if err := mutation.validate(i); err != nil {
			return err
		}
	}
	for _, mutation := range mutations {
		switch mutation.Kind {
		case MutationCreateAllocation:
			if err := d.store.insertAllocationAssignmentTx(ctx, tx, mutation.Allocation); err != nil {
				return err
			}
		case MutationChangeIntent, MutationBeginDrain:
			if err := d.store.setAllocationStateTx(ctx, tx, mutation.State); err != nil {
				return err
			}
		case MutationCompleteDrain:
			if err := d.store.deleteAllocationAssignmentTx(ctx, tx, mutation.AllocationID); err != nil {
				return err
			}
		case MutationMarkLost:
			if err := d.store.markAssignmentLostTx(ctx, tx, mutation.AllocationID, mutation.Message, now); err != nil {
				return err
			}
		case MutationSetMessage:
			if err := d.store.setAssignmentMessageTx(ctx, tx, mutation.AllocationID, mutation.Message, now); err != nil {
				return err
			}
		case MutationRetarget:
			assignment := mutation.Allocation
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
				SET deployment_id = $2, desired_spec_revision = $3, desired_rollout_generation = $4,
				    intent = $5, intent_message = '', updated_at = $6 WHERE id = $1`,
				assignment.ID, assignment.DeploymentID, assignment.SpecRevision, assignment.RolloutGeneration,
				assignment.Intent, now); err != nil {
				return err
			}
			journal.RecordAssignment(ctx, assignment.ID)
		case MutationReserveAddress:
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
				SET allocation_ipv4 = $2, updated_at = $3 WHERE id = $1`,
				mutation.AllocationID, mutation.Allocation.IPv4, now); err != nil {
				return err
			}
			journal.RecordAssignment(ctx, mutation.AllocationID)
		default:
			return fmt.Errorf("allocation mutation has unknown kind %q", mutation.Kind)
		}
	}
	return nil
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
	var mutations []AllocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		mutations = append(mutations, AllocationMutation{Kind: MutationCompleteDrain, AllocationID: id})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}

func (d *Delivery) withdrawServiceAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND rollout_state NOT IN ($2, $3, $4) ORDER BY id FOR UPDATE`,
		serviceID, AllocationRolloutWithdrawing, AllocationRolloutDraining, AllocationRolloutLost)
	if err != nil {
		return 0, err
	}
	var mutations []AllocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		mutations = append(mutations, AllocationMutation{Kind: MutationChangeIntent, State: allocationAssignmentState{
			AllocationID: id, RolloutState: AllocationRolloutWithdrawing, Intent: allocationIntentRun,
			IntentMessage: message, UpdatedAt: now,
		}})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return int64(len(mutations)), d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}

func (d *Delivery) retargetAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, agentID, deploymentID string, specRevision, generation int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND agent_id = $2 AND rollout_state IN ($3, $4) ORDER BY id FOR UPDATE`,
		serviceID, agentID, AllocationRolloutStarting, AllocationRolloutServing)
	if err != nil {
		return err
	}
	var mutations []AllocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		mutations = append(mutations, AllocationMutation{Kind: MutationRetarget, Allocation: AllocationAssignment{
			ID: id, DeploymentID: deploymentID, SpecRevision: specRevision, RolloutGeneration: generation, Intent: allocationIntentRun,
		}})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}
