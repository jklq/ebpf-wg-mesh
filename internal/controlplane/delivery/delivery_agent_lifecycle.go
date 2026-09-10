package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"time"
)

func (d *Delivery) SetAgentLifecycle(ctx context.Context, userID, agentID string, target AgentLifecycleState) (AgentRecord, []string, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return AgentRecord{}, nil, err
	}
	if target != AgentStateActive && target != AgentStateCordoned && target != AgentStateDraining && target != AgentStateRetired {
		return AgentRecord{}, nil, fmt.Errorf("%w: operators may set active, cordoned, draining, or retired", ErrInvalidAgentTransition)
	}
	var rec AgentRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := agentByIDQuerier(ctx, tx, agentID, false)
		if err != nil {
			return err
		}
		var administrationState AgentLifecycleState
		if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM agent_administration WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&administrationState); err != nil {
			return err
		}
		if err := validateAgentTransition(administrationState, target); err != nil {
			return err
		}
		now := time.Now().UTC()
		if target == AgentStateRetired {
			var allocations int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE agent_id = $1 AND rollout_state <> $2`, agentID, AllocationRolloutLost).Scan(&allocations); err != nil {
				return err
			}
			if allocations != 0 {
				return fmt.Errorf("%w: %d allocations remain", ErrAgentHasAllocations, allocations)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_bootstrap_tokens SET consumed_at = $1 WHERE agent_id = $2 AND consumed_at IS NULL`, now, agentID); err != nil {
				return err
			}
			if err := s.setAgentAdministrationTx(ctx, tx, AgentAdministration{
				AgentID: agentID, LifecycleState: AgentStateRetired, OperatorIntent: "retire",
				MaintenanceMessage:  "credentials revoked; mesh identity removed",
				CredentialRevokedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now,
			}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET advertise_addr = '',
				workload_ipv4_subnet = '', workload_ipv6_subnet = '', wireguard_public_key = '',
				wireguard_listen_port = 0, wireguard_ipv6 = '', updated_at = $1 WHERE id = $2`, now, agentID); err != nil {
				return err
			}
			journal.RecordAgent(ctx, agentID)
		} else {
			message := ""
			if target == AgentStateCordoned {
				message = "cordoned; existing allocations continue, new placement is disabled"
			} else if target == AgentStateDraining {
				message = "draining stateless allocations"
			}
			if err := s.setAgentAdministrationTx(ctx, tx, AgentAdministration{
				AgentID: agentID, LifecycleState: target, OperatorIntent: string(target),
				MaintenanceMessage: message, CredentialRevokedAt: current.CredentialRevokedAt, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		rec, err = agentByIDQuerier(ctx, tx, agentID, false)
		return err
	})
	if err != nil {
		return AgentRecord{}, nil, err
	}
	var notify []string
	if target == AgentStateDraining {
		notify, err = d.reconcileDrainingAgent(ctx, agentID)
		if err != nil {
			return AgentRecord{}, nil, err
		}
		rec, err = s.agentByID(ctx, agentID)
	} else if target == AgentStateActive {
		if err = d.ReconcileFleetCapacity(ctx); err != nil {
			return AgentRecord{}, nil, err
		}
		notify, err = s.agentIDs(ctx)
	} else if target == AgentStateRetired {
		notify, err = s.activeAgentIDs(ctx)
	}
	return rec, notify, err
}
