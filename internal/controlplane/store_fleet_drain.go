package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/restartpolicy"
)

// reconcileDrainingAgent moves stateless allocations through the same fenced
// allocation/deployment transition used by node-loss failover. Volume-backed
// allocations remain fenced in place until the Stage 7 handoff contract exists.
func (s *Store) reconcileDrainingAgent(ctx context.Context, agentID string) ([]string, error) {
	var notify []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agent, err := agentByIDQuerier(ctx, tx, agentID, true)
		if err != nil {
			return err
		}
		if agent.LifecycleState != agentStateDraining {
			return nil
		}
		allocations, err := listAllocationsForFailover(ctx, tx, agentID)
		if err != nil {
			return err
		}
		blocked := make(map[string]struct{})
		moved := false
		now := time.Now().UTC()
		for _, allocation := range allocations {
			service, err := s.serviceByIDInternalQuerier(ctx, tx, allocation.ServiceID)
			if err != nil {
				return err
			}
			if volumeName := serviceVolumeName(service.Spec); volumeName != "" {
				blocked[fmt.Sprintf("stateful allocation %s is fenced to volume %q until Stage 7 handoff is available", allocation.ID, volumeName)] = struct{}{}
				continue
			}
			occupied := map[string]struct{}{agentID: {}}
			rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM allocations WHERE service_id = $1 AND id <> $2`, allocation.ServiceID, allocation.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				occupied[id] = struct{}{}
			}
			if err := rows.Close(); err != nil {
				return err
			}
			destination, err := s.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied)
			if errors.Is(err, errNoPlacementAvailable) {
				blocked[strings.TrimSpace(strings.TrimPrefix(err.Error(), errNoPlacementAvailable.Error()+": "))] = struct{}{}
				continue
			}
			if err != nil {
				return err
			}
			nodeLoss, err := encodeRestartObservation(restartpolicy.NodeLossObservation(now, 0, 0))
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE allocations SET agent_id = $1,
				applied_spec_revision = 0, applied_rollout_generation = 0, phase = 'Pending',
				message = $2, allocation_ip = '', healthy_ports = '[]', healthy = FALSE,
				restart_observation_json = $3, updated_at = $4 WHERE id = $5 AND agent_id = $6`,
				destination, fmt.Sprintf("maintenance drain moved allocation from %s to %s", agentID, destination),
				nodeLoss, now, allocation.ID, agentID)
			if err != nil {
				return err
			}
			if affected, err := result.RowsAffected(); err != nil || affected != 1 {
				if err != nil {
					return err
				}
				return errConcurrentUpdate
			}
			if current, ok, err := s.currentDeploymentTx(ctx, tx, allocation.ServiceID); err != nil {
				return err
			} else if ok && deploymentTransitionAllowed(current.State, deploymentStateScheduling) {
				if _, err := s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
					ToState: deploymentStateScheduling, Actor: deploymentActor{Kind: deploymentCauseSystem},
					ReasonCode: reasonFailoverRescheduled,
					Detail:     fmt.Sprintf("Maintenance drain moved allocation from %s to %s", agentID, destination),
				}); err != nil {
					return err
				}
			}
			moved = true
		}
		remaining := len(allocations)
		if moved {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE agent_id = $1`, agentID).Scan(&remaining); err != nil {
				return err
			}
			if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT id FROM agents WHERE lifecycle_state <> 'retired' ORDER BY id`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				notify = append(notify, id)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		message := "drain complete; no allocations or attachments remain; safe to retire"
		if remaining > 0 {
			reasons := make([]string, 0, len(blocked))
			for reason := range blocked {
				if reason != "" {
					reasons = append(reasons, reason)
				}
			}
			sort.Strings(reasons)
			message = fmt.Sprintf("drain paused with %d allocation(s): %s", remaining, strings.Join(reasons, "; "))
		}
		_, err = tx.ExecContext(ctx, `UPDATE agents SET maintenance_message = $1, updated_at = $2 WHERE id = $3 AND lifecycle_state = 'draining'`, message, now, agentID)
		return err
	})
	return notify, err
}

// reconcileFleetCapacity retries pending replica placement and interrupted
// drains whenever observed fleet capacity changes (for example a node return).
func (s *Store) reconcileFleetCapacity(ctx context.Context) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE placement_message <> '' ORDER BY id FOR UPDATE`)
		if err != nil {
			return err
		}
		var serviceIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			serviceIDs = append(serviceIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		changed := false
		for _, serviceID := range serviceIDs {
			service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
			if err != nil {
				return err
			}
			before, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, false)
			if err != nil {
				return err
			}
			after, err := s.reconcileServiceReplicasTx(ctx, tx, service, "", time.Now().UTC())
			if err != nil {
				return err
			}
			changed = changed || len(before) != len(after)
		}
		if changed {
			return s.bumpAllDesiredRevisionsTx(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return err
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if agent.LifecycleState == agentStateDraining {
			if _, err := s.reconcileDrainingAgent(ctx, agent.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
