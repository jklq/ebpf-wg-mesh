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

	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		agents, services, err := s.schedulerSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if len(services) == 0 {
			return nil
		}

		allocations, err := allocationStatesForFailover(ctx, s, tx)
		if err != nil {
			return err
		}

		healthyAgents := make(map[string]AgentRecord, len(agents))
		for _, agent := range agents {
			if agent.LastSeenAt.After(cutoff) && agent.StateBeforeUnavailable == AgentStateActive {
				healthyAgents[agent.ID] = agent
			}
		}
		servicesByID := make(map[string]*ServiceRecord, len(services))
		for i := range services {
			servicesByID[services[i].ID] = &services[i]
		}

		moved := false
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
			if replacement.Blocked {
				result.BlockedServiceIDs = append(result.BlockedServiceIDs, service.ID)
				continue
			}
			if replacement.Replaced {
				result.MovedServiceIDs = append(result.MovedServiceIDs, service.ID)
			}
		}

		if moved {
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

func allocationStatesForFailover(ctx context.Context, s *persistence, q ServiceQueryer) ([]AllocationRecord, error) {
	return listAllocationsForFailover(ctx, s, q, "")
}

func listAllocationsForFailover(ctx context.Context, s *persistence, q ServiceQueryer, agentID string) ([]AllocationRecord, error) {
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
		out = append(out, s.overlayAllocation(rec))
	}
	return out, rows.Err()
}

func (s *persistence) markAllocationUnavailableForFailoverTx(ctx context.Context, tx *sql.Tx, allocationID string, current allocationFailoverState, message string, now time.Time) (bool, error) {
	if current.phase == allocationPhaseUnavailable && current.message == message && len(current.healthyIPv4Ports) == 0 && len(current.healthyIPv6Ports) == 0 && !current.healthy {
		return false, nil
	}
	err := (&Delivery{store: s}).applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{
		Kind: DecisionSetMessage, AllocationID: allocationID, Message: message,
	}))
	return err == nil, err
}

func (d *Delivery) ReconcileFailover(ctx context.Context, unhealthyThreshold time.Duration) (ServiceFailoverResult, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
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
