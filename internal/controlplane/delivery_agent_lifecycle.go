package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (d *Delivery) setAgentLifecycle(ctx context.Context, userID, agentID string, target agentLifecycleState) (agentRecord, []string, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return agentRecord{}, nil, err
	}
	if target != agentStateActive && target != agentStateCordoned && target != agentStateDraining && target != agentStateRetired {
		return agentRecord{}, nil, fmt.Errorf("%w: operators may set active, cordoned, draining, or retired", errInvalidAgentTransition)
	}
	var rec agentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := agentByIDQuerier(ctx, tx, agentID, true)
		if err != nil {
			return err
		}
		if err := validateAgentTransition(current.LifecycleState, target); err != nil {
			return err
		}
		now := time.Now().UTC()
		if target == agentStateRetired {
			var allocations int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE agent_id = $1 AND rollout_state <> $2`, agentID, allocationRolloutLost).Scan(&allocations); err != nil {
				return err
			}
			if allocations != 0 {
				return fmt.Errorf("%w: %d allocations remain", errAgentHasAllocations, allocations)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_bootstrap_tokens SET consumed_at = $1 WHERE agent_id = $2 AND consumed_at IS NULL`, now, agentID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET lifecycle_state = 'retired',
				credential_revoked_at = $1, maintenance_message = 'credentials revoked; mesh identity removed',
				advertise_addr = '', workload_ipv4_subnet = '', workload_ipv6_subnet = '', wireguard_public_key = '',
				wireguard_listen_port = 0, wireguard_ipv6 = '', updated_at = $1 WHERE id = $2`, now, agentID); err != nil {
				return err
			}
			if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
				return err
			}
		} else {
			message := ""
			if target == agentStateCordoned {
				message = "cordoned; existing allocations continue, new placement is disabled"
			} else if target == agentStateDraining {
				message = "draining stateless allocations"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET lifecycle_state = $1,
				state_before_unavailable = '', maintenance_message = $2, updated_at = $3 WHERE id = $4`, target, message, now, agentID); err != nil {
				return err
			}
		}
		rec, err = agentByIDQuerier(ctx, tx, agentID, false)
		return err
	})
	if err != nil {
		return agentRecord{}, nil, err
	}
	var notify []string
	if target == agentStateDraining {
		notify, err = d.reconcileDrainingAgent(ctx, agentID)
		if err != nil {
			return agentRecord{}, nil, err
		}
		rec, err = s.agentByID(ctx, agentID)
	} else if target == agentStateActive {
		if err = d.ReconcileFleetCapacity(ctx); err != nil {
			return agentRecord{}, nil, err
		}
		notify, err = s.agentIDs(ctx)
	} else if target == agentStateRetired {
		notify, err = s.activeAgentIDs(ctx)
	}
	return rec, notify, err
}
