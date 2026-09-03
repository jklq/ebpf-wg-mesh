package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

const allocationPhaseUnavailable = "Unavailable"

type serviceFailoverResult struct {
	MovedServiceIDs   []string
	BlockedServiceIDs []string
	NotifyAgentIDs    []string
	EnvironmentIDs    []string
	IngressChanged    bool
}

type allocationFailoverState struct {
	phase        string
	message      string
	allocationIP string
	healthyPorts jsonInt32Slice
	healthy      bool
}

func (s *Store) failoverUnhealthyServices(ctx context.Context, now time.Time, unhealthyThreshold time.Duration) (serviceFailoverResult, error) {
	var result serviceFailoverResult
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
			RETURNING id`, agentStateUnavailable, now.UTC(), agentStateActive, agentStateCordoned, agentStateDraining, cutoff)
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
				if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
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

		healthyAgents := make(map[string]agentRecord, len(agents))
		for _, agent := range agents {
			if agent.LastSeenAt.After(cutoff) && agent.LifecycleState == agentStateActive {
				healthyAgents[agent.ID] = agent
			}
		}
		servicesByID := make(map[string]*serviceRecord, len(services))
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
			replacement, err := s.replaceLostNodeAllocationTx(ctx, tx, allocation.AgentID, allocation, now.UTC())
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
			if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
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
		return serviceFailoverResult{}, err
	}
	return result, nil
}

func finishLostDrainingAllocationTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = 'Drained',
		        message = $1,
		        healthy = FALSE,
		        healthy_ports = $2,
		        updated_at = $3
		  WHERE id = $4`,
		message, []byte("[]"), now, allocationID,
	)
	return err
}

func allocationStatesForFailover(ctx context.Context, q serviceQueryer) ([]allocationRecord, error) {
	return listAllocationsForFailover(ctx, q, "")
}

func listAllocationsForFailover(ctx context.Context, q serviceQueryer, agentID string) ([]allocationRecord, error) {
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
	var out []allocationRecord
	for rows.Next() {
		rec, err := scanAllocationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func markAllocationUnavailableForFailover(ctx context.Context, q serviceQueryer, allocationID string, current allocationFailoverState, message string, now time.Time) (bool, error) {
	if current.phase == allocationPhaseUnavailable && current.message == message && current.allocationIP == "" && len(current.healthyPorts) == 0 && !current.healthy {
		return false, nil
	}
	_, err := q.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = $1, message = $2, allocation_ip = '', healthy_ports = $3, healthy = FALSE, updated_at = $4
		  WHERE id = $5`,
		allocationPhaseUnavailable, message, []byte("[]"), now, allocationID,
	)
	return err == nil, err
}

type failoverNotifier interface {
	Notify(agentID string)
}

type failoverIngress interface {
	RequestSync()
}

type ServiceFailoverReconciler struct {
	store              *Store
	notifier           failoverNotifier
	ingress            failoverIngress
	events             *PlatformEvents
	interval           time.Duration
	unhealthyThreshold time.Duration
	now                func() time.Time
}

func NewServiceFailoverReconciler(store *Store, notifier failoverNotifier, ingress failoverIngress, events *PlatformEvents, interval, unhealthyThreshold time.Duration) *ServiceFailoverReconciler {
	return &ServiceFailoverReconciler{
		store: store, notifier: notifier, ingress: ingress, events: events,
		interval: interval, unhealthyThreshold: unhealthyThreshold,
	}
}

func (r *ServiceFailoverReconciler) Reconcile(ctx context.Context) (serviceFailoverResult, error) {
	if r == nil || r.store == nil {
		return serviceFailoverResult{}, nil
	}
	now, err := databaseTime(ctx, r.store.db)
	if r.now != nil {
		now = r.now().UTC()
		err = nil
	}
	if err != nil {
		return serviceFailoverResult{}, fmt.Errorf("read database time: %w", err)
	}
	result, err := r.store.failoverUnhealthyServices(ctx, now, r.unhealthyThreshold)
	if err != nil {
		return serviceFailoverResult{}, err
	}
	for _, agentID := range result.NotifyAgentIDs {
		if r.notifier != nil {
			r.notifier.Notify(agentID)
		}
	}
	if result.IngressChanged && r.ingress != nil {
		r.ingress.RequestSync()
	}
	for _, environmentID := range result.EnvironmentIDs {
		if r.events != nil {
			if _, err := r.events.Publish(ctx, environmentID); err != nil {
				return serviceFailoverResult{}, fmt.Errorf("publish failover event: %w", err)
			}
		}
	}
	return result, nil
}

func (r *ServiceFailoverReconciler) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.interval <= 0 {
		return fmt.Errorf("failover reconcile interval must be greater than zero")
	}
	reconcile := func() {
		result, err := r.Reconcile(ctx)
		if err != nil {
			slog.Warn("service failover reconcile failed", "error", err)
			return
		}
		if len(result.MovedServiceIDs) > 0 || len(result.BlockedServiceIDs) > 0 {
			slog.Info("service failover reconciled", "moved", len(result.MovedServiceIDs), "blocked", len(result.BlockedServiceIDs))
		}
	}
	reconcile()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			reconcile()
		}
	}
}
