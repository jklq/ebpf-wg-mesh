package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// reconcileDrainingAgent replaces stateless allocations on a draining node by
// starting targeted rolling replacements. Volume-backed allocations remain
// fenced until the Stage 7 handoff contract exists. Existing allocation rows
// are never rewritten onto another agent.
func (s *Store) reconcileDrainingAgent(ctx context.Context, agentID string) ([]string, error) {
	var notify []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		notify = nil
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
		started := false
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
			if allocation.RolloutState == allocationRolloutWithdrawing || allocation.RolloutState == allocationRolloutDraining {
				continue
			}
			replacing, err := allocationReplacementInProgressTx(ctx, tx, service, allocation)
			if err != nil {
				return err
			}
			if replacing {
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
			if _, err := s.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied); errors.Is(err, errNoPlacementAvailable) {
				blocked[strings.TrimSpace(strings.TrimPrefix(err.Error(), errNoPlacementAvailable.Error()+": "))] = struct{}{}
				continue
			} else if err != nil {
				return err
			}
			current, ok, err := s.currentDeploymentTx(ctx, tx, allocation.ServiceID)
			if err != nil {
				return err
			}
			if !ok || current.ResolvedSpec == nil || strings.TrimSpace(current.ImageDigest) == "" {
				blocked["service has no reusable image snapshot for a rolling replacement"] = struct{}{}
				continue
			}
			detail := fmt.Sprintf("Maintenance drain replacing allocation %s from %s", allocation.ID, agentID)
			if _, err := s.copyDeploymentRolloutTargetTx(ctx, tx, service, current, "", reasonAgentDrain, detail, allocation.ID); err != nil {
				switch {
				case errors.Is(err, errRolloutInProgress):
					blocked["waiting for an in-progress rollout to finish"] = struct{}{}
				case errors.Is(err, errVolumeRollingUnsupported):
					blocked[fmt.Sprintf("stateful allocation %s cannot overlap generations until Stage 7 handoff is available", allocation.ID)] = struct{}{}
				case errors.Is(err, errDeploymentActionInvalid), errors.Is(err, sql.ErrNoRows):
					blocked[err.Error()] = struct{}{}
				default:
					return err
				}
				continue
			}
			started = true
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE agent_id = $1`, agentID).Scan(&remaining); err != nil {
			return err
		}
		message := "drain complete; no allocations or attachments remain; safe to retire"
		switch {
		case remaining > 0 && len(blocked) > 0:
			reasons := make([]string, 0, len(blocked))
			for reason := range blocked {
				if reason != "" {
					reasons = append(reasons, reason)
				}
			}
			sort.Strings(reasons)
			message = fmt.Sprintf("drain paused with %d allocation(s): %s", remaining, strings.Join(reasons, "; "))
		case remaining > 0:
			message = fmt.Sprintf("drain in progress: replacing %d allocation(s) through rolling replacement", remaining)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET maintenance_message = $1, updated_at = $2 WHERE id = $3 AND lifecycle_state = 'draining'`, message, now, agentID); err != nil {
			return err
		}
		if !started {
			return nil
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
		return rows.Close()
	})
	return notify, err
}

func allocationReplacementInProgressTx(ctx context.Context, tx *sql.Tx, service serviceRecord, allocation allocationRecord) (bool, error) {
	if allocation.DesiredRolloutGeneration >= service.RolloutGeneration {
		return false, nil
	}
	rollout, ok, err := loadCurrentRolloutTx(ctx, tx, service)
	if err != nil || !ok {
		return false, err
	}
	if rollout.State != rolloutStateInProgress && rollout.State != rolloutStatePendingBuild {
		return false, nil
	}
	return rollout.TargetAllocationID == "" || rollout.TargetAllocationID == allocation.ID, nil
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
