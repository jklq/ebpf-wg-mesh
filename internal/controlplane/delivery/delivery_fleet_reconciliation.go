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

func (d *Delivery) reconcileDrainingAgent(ctx context.Context, agentID string) ([]string, error) {
	s := d.store
	var notify []string
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		notify = nil
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM agent_administration WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&locked); err != nil {
			return err
		}
		agent, err := agentByIDQuerier(ctx, tx, locked, false)
		if err != nil {
			return err
		}
		if agent.LifecycleState != AgentStateDraining {
			return nil
		}
		allocations, err := listAllocationsForFailover(ctx, s, tx, agentID)
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
			if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
				blocked[fmt.Sprintf("stateful allocation %s is fenced to volume %q until Stage 7 handoff is available", allocation.ID, volumeName)] = struct{}{}
				continue
			}
			if allocation.RolloutState == AllocationRolloutWithdrawing || allocation.RolloutState == AllocationRolloutDraining {
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
			if _, err := d.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied); errors.Is(err, ErrNoPlacementAvailable) {
				blocked[strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrNoPlacementAvailable.Error()+": "))] = struct{}{}
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
			if _, err := d.copyDeploymentRolloutTargetTx(ctx, tx, service, current, "", reasonAgentDrain, detail, allocation.ID); err != nil {
				switch {
				case errors.Is(err, ErrRolloutInProgress):
					blocked["waiting for an in-progress rollout to finish"] = struct{}{}
				case errors.Is(err, ErrVolumeRollingUnsupported):
					blocked[fmt.Sprintf("stateful allocation %s cannot overlap generations until Stage 7 handoff is available", allocation.ID)] = struct{}{}
				case errors.Is(err, ErrDeploymentActionInvalid), errors.Is(err, sql.ErrNoRows):
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
		if err := s.setAgentMaintenanceMessageTx(ctx, tx, agentID, message, now); err != nil {
			return err
		}
		if !started {
			return nil
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

func allocationReplacementInProgressTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, allocation AllocationRecord) (bool, error) {
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

func (d *Delivery) ReconcileFleetCapacity(ctx context.Context) error {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
			after, err := d.reconcileServiceReplicasTx(ctx, tx, service, "", time.Now().UTC())
			if err != nil {
				return err
			}
			changed = changed || len(before) != len(after)
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
	cutoff := time.Now().UTC().Add(-AgentHealthyTTL)
	for _, agent := range agents {
		switch agent.LifecycleState {
		case AgentStateDraining:
			if _, err := d.reconcileDrainingAgent(ctx, agent.ID); err != nil {
				return err
			}
		case AgentStateUnavailable:
			if _, _, err := d.failoverServicesFromAgent(ctx, agent.ID, cutoff); err != nil {
				return err
			}
		}
	}
	return nil
}
