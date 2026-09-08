package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/restartpolicy"
	"errors"
	"fmt"
	"strings"
	"time"
)

type nodeLossReplacementResult struct {
	Replaced bool
	Blocked  bool
	Changed  bool
	Bumped   bool
}

// failoverServicesFromAgent evaluates only allocations currently assigned to
// the expired agent. The last-seen check and replacement share a serializable
// transaction so a concurrent heartbeat prevents stale failover.
//
// Existing allocation rows are never rewritten onto another agent. The dead
// node's allocation is marked lost/unavailable and a new allocation is placed
// through rolling replacement, matching fleet drain.
func (d *Delivery) failoverServicesFromAgent(ctx context.Context, agentID string, cutoff time.Time) ([]string, []string, error) {
	s := d.store
	var notifyAgentIDs []string
	var changedEnvironmentIDs []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		notifyAgentIDs = nil
		changedEnvironmentIDs = nil
		var lastSeen time.Time
		if err := tx.QueryRowContext(ctx, `SELECT last_seen_at FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&lastSeen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if lastSeen.After(cutoff) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET
			state_before_unavailable = CASE WHEN lifecycle_state IN ('active', 'cordoned', 'draining') THEN lifecycle_state ELSE state_before_unavailable END,
			lifecycle_state = CASE WHEN lifecycle_state = 'retired' THEN lifecycle_state ELSE 'unavailable' END,
			maintenance_message = CASE WHEN lifecycle_state = 'retired' THEN maintenance_message ELSE 'heartbeat expired; workloads are being failed over' END,
			updated_at = $1 WHERE id = $2`, time.Now().UTC(), agentID); err != nil {
			return err
		}

		allocations, err := listAllocationsForFailover(ctx, tx, agentID)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		needBump := false
		alreadyBumped := false
		changedEnvironments := make(map[string]struct{})
		for _, allocation := range allocations {
			result, err := d.replaceLostNodeAllocationTx(ctx, tx, agentID, allocation, now)
			if err != nil {
				return err
			}
			if !result.Changed {
				continue
			}
			needBump = true
			changedEnvironments[allocation.EnvironmentID] = struct{}{}
			if result.Bumped {
				alreadyBumped = true
			}
		}

		if needBump && !alreadyBumped {
			if err := dbtx.BumpAllDesiredRevisions(ctx, tx); err != nil {
				return err
			}
		}
		if needBump {
			agentRows, err := tx.QueryContext(ctx, `SELECT id FROM agents ORDER BY id`)
			if err != nil {
				return err
			}
			for agentRows.Next() {
				var id string
				if err := agentRows.Scan(&id); err != nil {
					agentRows.Close()
					return err
				}
				notifyAgentIDs = append(notifyAgentIDs, id)
			}
			if err := agentRows.Close(); err != nil {
				return err
			}
		}
		for environmentID := range changedEnvironments {
			changedEnvironmentIDs = append(changedEnvironmentIDs, environmentID)
		}
		return nil
	})
	return notifyAgentIDs, changedEnvironmentIDs, err
}

// replaceLostNodeAllocationTx fences or replaces one allocation on a dead node.
// It never rewrites allocations.agent_id. Volume-backed and managed workloads
// stay pinned; stateless replacements use the same targeted rolling path as drain.
func (d *Delivery) replaceLostNodeAllocationTx(ctx context.Context, tx *sql.Tx, deadAgentID string, allocation AllocationRecord, now time.Time) (nodeLossReplacementResult, error) {
	s := d.store
	var result nodeLossReplacementResult
	switch lostAllocationDisposition(allocation, deadAgentID) {
	case failoverIgnore:
		return result, nil
	case failoverFinishDrain:
		if err := finishLostDrainingAllocationTx(ctx, tx, allocation.ID, "node lost while draining; allocation will be removed", now); err != nil {
			return result, err
		}
		return nodeLossReplacementResult{Changed: true, Replaced: true}, nil
	}

	service, err := s.serviceByIDInternalQuerier(ctx, tx, allocation.ServiceID)
	if err != nil {
		return result, err
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
		changed, err := markAllocationUnavailableForFailover(ctx, tx, allocation.ID, state, decision.Message, now)
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

	if err := markAllocationLostTx(ctx, tx, allocation.ID, lostMessage, now); err != nil {
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

func markAllocationLostTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	nodeLoss, err := encodeRestartObservation(restartpolicy.NodeLossObservation(now, 0, 0))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = $1,
		        rollout_state = $2,
		        message = $3,
		        allocation_ipv4 = '',
		        allocation_ipv6 = '',
		        healthy_ipv4_ports = $4,
		        healthy_ipv6_ports = $4,
		        healthy = FALSE,
		        restart_observation_json = $5,
		        updated_at = $6
		  WHERE id = $7`,
		allocationPhaseUnavailable, AllocationRolloutLost, message, []byte("[]"), nodeLoss, now, allocationID,
	)
	return err
}
