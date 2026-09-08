package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"fmt"
	"sort"
	"time"
)

const allocationPhaseUnavailable = "Unavailable"

type ServiceFailoverResult struct {
	MovedServiceIDs   []string
	BlockedServiceIDs []string
	NotifyAgentIDs    []string
	EnvironmentIDs    []string
	IngressChanged    bool
}

type allocationFailoverState struct {
	phase            string
	message          string
	healthyIPv4Ports jsonInt32Slice
	healthyIPv6Ports jsonInt32Slice
	healthy          bool
}

func (d *Delivery) failoverUnhealthyServices(ctx context.Context, now time.Time, unhealthyThreshold time.Duration) (ServiceFailoverResult, error) {
	s := d.store
	var result ServiceFailoverResult
	if unhealthyThreshold <= 0 {
		return result, fmt.Errorf("unhealthy threshold must be greater than zero")
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Serialize placement repair with concurrent repair loops. CockroachDB's
		// serializable retry then also protects capacity reads from service creates.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM services ORDER BY id FOR UPDATE`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}

		cutoff := now.UTC().Add(-unhealthyThreshold)
		staleRows, err := tx.QueryContext(ctx, `UPDATE agents
			SET state_before_unavailable = lifecycle_state,
			    lifecycle_state = $1,
			    updated_at = $2
			WHERE lifecycle_state IN ($3, $4, $5) AND last_seen_at <= $6
			RETURNING id`, AgentStateUnavailable, now.UTC(), AgentStateActive, AgentStateCordoned, AgentStateDraining, cutoff)
		if err != nil {
			return err
		}
		var staleAgentIDs []string
		for staleRows.Next() {
			var agentID string
			if err := staleRows.Scan(&agentID); err != nil {
				_ = staleRows.Close()
				return err
			}
			staleAgentIDs = append(staleAgentIDs, agentID)
		}
		if err := staleRows.Err(); err != nil {
			_ = staleRows.Close()
			return err
		}
		if err := staleRows.Close(); err != nil {
			return err
		}

		agents, services, err := s.schedulerSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if len(services) == 0 {
			if len(staleAgentIDs) > 0 {
				if err := dbtx.BumpAllDesiredRevisions(ctx, tx); err != nil {
					return err
				}
				for _, agent := range agents {
					result.NotifyAgentIDs = append(result.NotifyAgentIDs, agent.ID)
				}
			}
			return nil
		}

		allocations, err := allocationStatesForFailover(ctx, tx)
		if err != nil {
			return err
		}

		healthyAgents := make(map[string]AgentRecord, len(agents))
		for _, agent := range agents {
			if agent.LastSeenAt.After(cutoff) && agent.LifecycleState == AgentStateActive {
				healthyAgents[agent.ID] = agent
			}
		}
		servicesByID := make(map[string]*ServiceRecord, len(services))
		for i := range services {
			servicesByID[services[i].ID] = &services[i]
		}

		moved := false
		alreadyBumped := false
		changedEnvironments := make(map[string]struct{})
		for i := range allocations {
			allocation := allocations[i]
			service := servicesByID[allocation.ServiceID]
			if service == nil {
				return fmt.Errorf("allocation %s has no service", allocation.ID)
			}
			if _, healthy := healthyAgents[allocation.AgentID]; healthy {
				continue
			}
			replacement, err := d.replaceLostNodeAllocationTx(ctx, tx, allocation.AgentID, allocation, now.UTC())
			if err != nil {
				return err
			}
			if !replacement.Changed {
				continue
			}
			result.IngressChanged = true
			changedEnvironments[service.EnvironmentID] = struct{}{}
			moved = true
			if replacement.Bumped {
				alreadyBumped = true
			}
			if replacement.Blocked {
				result.BlockedServiceIDs = append(result.BlockedServiceIDs, service.ID)
				continue
			}
			if replacement.Replaced {
				result.MovedServiceIDs = append(result.MovedServiceIDs, service.ID)
			}
		}

		if (moved || len(staleAgentIDs) > 0) && !alreadyBumped {
			if err := dbtx.BumpAllDesiredRevisions(ctx, tx); err != nil {
				return err
			}
		}
		if moved || len(staleAgentIDs) > 0 {
			for _, agent := range agents {
				result.NotifyAgentIDs = append(result.NotifyAgentIDs, agent.ID)
			}
			for environmentID := range changedEnvironments {
				result.EnvironmentIDs = append(result.EnvironmentIDs, environmentID)
			}
			sort.Strings(result.EnvironmentIDs)
		}
		return nil
	})
	if err != nil {
		return ServiceFailoverResult{}, err
	}
	return result, nil
}

func finishLostDrainingAllocationTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = 'Drained',
		        message = $1,
		        healthy = FALSE,
		        healthy_ipv4_ports = $2,
		        healthy_ipv6_ports = $2,
		        updated_at = $3
		  WHERE id = $4`,
		message, []byte("[]"), now, allocationID,
	)
	return err
}

func allocationStatesForFailover(ctx context.Context, q ServiceQueryer) ([]AllocationRecord, error) {
	return listAllocationsForFailover(ctx, q, "")
}

func listAllocationsForFailover(ctx context.Context, q ServiceQueryer, agentID string) ([]AllocationRecord, error) {
	query := allocationSelectSQL
	args := []any{}
	if agentID != "" {
		query += ` WHERE a.agent_id = $1`
		args = append(args, agentID)
	}
	query += ` ORDER BY s.created_at ASC, a.id ASC`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllocationRecord
	for rows.Next() {
		rec, err := scanAllocationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func markAllocationUnavailableForFailover(ctx context.Context, q ServiceQueryer, allocationID string, current allocationFailoverState, message string, now time.Time) (bool, error) {
	if current.phase == allocationPhaseUnavailable && current.message == message && len(current.healthyIPv4Ports) == 0 && len(current.healthyIPv6Ports) == 0 && !current.healthy {
		return false, nil
	}
	_, err := q.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = $1, message = $2, healthy_ipv4_ports = $3, healthy_ipv6_ports = $3, healthy = FALSE, updated_at = $4
		  WHERE id = $5`,
		allocationPhaseUnavailable, message, []byte("[]"), now, allocationID,
	)
	return err == nil, err
}

// ReconcileFailover detects unhealthy agents, applies placement repair in one
// transaction, then wakes agents and requests ingress sync.
func (d *Delivery) ReconcileFailover(ctx context.Context, unhealthyThreshold time.Duration) (ServiceFailoverResult, error) {
	if d == nil || d.store == nil {
		return ServiceFailoverResult{}, nil
	}
	now, err := dbtx.DatabaseTime(ctx, d.store.db)
	if d.failoverNow != nil {
		now = d.failoverNow().UTC()
		err = nil
	}
	if err != nil {
		return ServiceFailoverResult{}, fmt.Errorf("read database time: %w", err)
	}
	result, err := d.failoverUnhealthyServices(ctx, now, unhealthyThreshold)
	if err != nil {
		return ServiceFailoverResult{}, err
	}
	for _, agentID := range result.NotifyAgentIDs {
		if d.notifier != nil {
			d.notifier.Notify(agentID)
		}
	}
	if result.IngressChanged && d.ingress != nil {
		d.ingress.RequestSync()
	}
	return result, nil
}
