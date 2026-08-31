package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
)

// failoverServicesFromAgent evaluates only allocations currently assigned to
// the expired agent. The last-seen check and moves share a serializable
// transaction so a concurrent heartbeat prevents stale failover.
func (s *Store) failoverServicesFromAgent(ctx context.Context, agentID string, cutoff time.Time) ([]string, []string, error) {
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

		rows, err := tx.QueryContext(ctx,
			`SELECT a.id, a.service_id, a.phase, a.message, a.allocation_ip, a.healthy_ports, a.healthy,
			        a.rollout_state, s.environment_id, e.project_id, p.kind, r.spec_json
			   FROM allocations a
			   JOIN services s ON s.id = a.service_id
			   JOIN environments e ON e.id = s.environment_id
			   JOIN projects p ON p.id = e.project_id
			   JOIN service_revisions r
			     ON r.service_id = s.id
			    AND r.spec_revision = s.current_spec_revision
			  WHERE a.agent_id = $1
			  ORDER BY s.created_at, s.id
			  FOR UPDATE OF a`,
			agentID,
		)
		if err != nil {
			return err
		}
		type candidate struct {
			allocationID  string
			serviceID     string
			environmentID string
			projectKind   projectKind
			spec          *platformv1.ServiceSpec
			rolloutState  string
			state         allocationFailoverState
		}
		var services []candidate
		for rows.Next() {
			var rec candidate
			var projectID string
			var specJSON []byte
			if err := rows.Scan(
				&rec.allocationID,
				&rec.serviceID,
				&rec.state.phase,
				&rec.state.message,
				&rec.state.allocationIP,
				&rec.state.healthyPorts,
				&rec.state.healthy,
				&rec.rolloutState,
				&rec.environmentID,
				&projectID,
				&rec.projectKind,
				&specJSON,
			); err != nil {
				rows.Close()
				return err
			}
			rec.spec = &platformv1.ServiceSpec{}
			if err := protojson.Unmarshal(specJSON, rec.spec); err != nil {
				rows.Close()
				return fmt.Errorf("decode service %s spec: %w", rec.serviceID, err)
			}
			services = append(services, rec)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		now := time.Now().UTC()
		moved := false
		changedEnvironments := make(map[string]struct{})
		for _, service := range services {
			if service.rolloutState == allocationRolloutDraining || service.rolloutState == allocationRolloutWithdrawing {
				if err := finishLostDrainingAllocationTx(ctx, tx, service.allocationID, "node lost while draining; allocation will be removed", now); err != nil {
					return err
				}
				changedEnvironments[service.environmentID] = struct{}{}
				moved = true
				continue
			}
			blockedMessage := ""
			switch {
			case service.projectKind == projectKindManaged:
				blockedMessage = "agent unhealthy; managed/trusted workload remains pinned to its trusted agent"
			case serviceVolumeName(service.spec) != "":
				blockedMessage = fmt.Sprintf("agent unhealthy; service remains pinned because node-bound volume %q requires replicated storage before failover", serviceVolumeName(service.spec))
			}

			destination := ""
			if blockedMessage == "" {
				occupied := map[string]struct{}{}
				otherRows, err := tx.QueryContext(ctx, `SELECT agent_id FROM allocations WHERE service_id = $1 AND id <> $2`, service.serviceID, service.allocationID)
				if err != nil {
					return err
				}
				for otherRows.Next() {
					var agentID string
					if err := otherRows.Scan(&agentID); err != nil {
						otherRows.Close()
						return err
					}
					occupied[agentID] = struct{}{}
				}
				if err := otherRows.Close(); err != nil {
					return err
				}
				destination, err = s.chooseAgentForReplicaQuerier(ctx, tx, service.spec, occupied)
				if errors.Is(err, errNoPlacementAvailable) {
					blockedMessage = "agent unhealthy; automatic failover blocked because no healthy non-reserved agent has sufficient capacity"
				} else if err != nil {
					return err
				}
			}
			if blockedMessage != "" {
				changed, err := markAllocationUnavailableForFailover(ctx, tx, service.allocationID, service.state, blockedMessage, now)
				if err != nil {
					return err
				}
				if changed {
					changedEnvironments[service.environmentID] = struct{}{}
				}
				continue
			}
			if destination == agentID {
				return fmt.Errorf("expired agent %s selected for service %s", agentID, service.serviceID)
			}
			nodeLoss, err := encodeRestartObservation(restartpolicy.NodeLossObservation(now, 0, 0))
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET agent_id = $1,
				        applied_spec_revision = 0,
				        applied_rollout_generation = 0,
				        phase = 'Pending',
				        message = $2,
				        allocation_ip = '',
				        healthy_ports = '[]',
				        healthy = FALSE,
				        restart_observation_json = $3,
				        updated_at = $4
				  WHERE id = $5 AND agent_id = $6`,
				destination,
				fmt.Sprintf("rescheduled from expired agent %s to %s", agentID, destination),
				nodeLoss,
				now,
				service.allocationID,
				agentID,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return errConcurrentUpdate
			}
			moved = true
			changedEnvironments[service.environmentID] = struct{}{}
			if current, ok, err := s.currentDeploymentTx(ctx, tx, service.serviceID); err != nil {
				return err
			} else if ok {
				if _, err := s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
					ToState:    deploymentStateScheduling,
					Actor:      deploymentActor{Kind: deploymentCauseSystem},
					ReasonCode: reasonFailoverRescheduled,
					Detail:     fmt.Sprintf("Rescheduled from expired agent %s to %s", agentID, destination),
				}); err != nil {
					return err
				}
			}
		}

		if moved {
			if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
				return err
			}
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
