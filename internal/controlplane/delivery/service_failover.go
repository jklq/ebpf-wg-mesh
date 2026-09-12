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
	var result ServiceFailoverResult
	if unhealthyThreshold <= 0 {
		return result, fmt.Errorf("unhealthy threshold must be greater than zero")
	}
	cutoff := now.UTC().Add(-unhealthyThreshold)
	changedEnvironments := make(map[string]struct{})
	for _, agent := range d.live.Agents() {
		if agent.LifecycleState != AgentStateUnavailable || agent.StateBeforeUnavailable != AgentStateActive || agent.LastSeenAt.After(cutoff) {
			continue
		}
		agentResult, err := d.failoverAgent(ctx, agent.ID, cutoff, now.UTC())
		if err != nil {
			return ServiceFailoverResult{}, err
		}
		result.MovedServiceIDs = append(result.MovedServiceIDs, agentResult.MovedServiceIDs...)
		result.BlockedServiceIDs = append(result.BlockedServiceIDs, agentResult.BlockedServiceIDs...)
		result.IngressChanged = result.IngressChanged || agentResult.IngressChanged
		for _, environmentID := range agentResult.EnvironmentIDs {
			changedEnvironments[environmentID] = struct{}{}
		}
	}
	if result.IngressChanged {
		result.NotifyAgentIDs = d.live.AgentIDs()
		for environmentID := range changedEnvironments {
			result.EnvironmentIDs = append(result.EnvironmentIDs, environmentID)
		}
		sort.Strings(result.EnvironmentIDs)
	}
	return result, nil
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
