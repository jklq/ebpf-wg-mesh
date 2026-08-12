package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"
)

const allocationPhaseUnavailable = "Unavailable"

type serviceFailoverResult struct {
	MovedServiceIDs   []string
	BlockedServiceIDs []string
	NotifyAgentIDs    []string
	IngressChanged    bool
}

type allocationFailoverState struct {
	phase        string
	message      string
	allocationIP string
	healthyPorts jsonInt32Slice
	healthy      bool
}

type agentWorkloadUsage struct {
	services int
	cpu      int64
	memory   int64
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

		agents, services, err := s.schedulerSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if len(services) == 0 {
			return nil
		}

		projectKinds, err := projectKindsForFailover(ctx, tx)
		if err != nil {
			return err
		}
		allocations, err := allocationStatesForFailover(ctx, tx)
		if err != nil {
			return err
		}

		cutoff := now.UTC().Add(-unhealthyThreshold)
		healthyAgents := make(map[string]agentRecord, len(agents))
		usage := make(map[string]*agentWorkloadUsage, len(agents))
		for _, agent := range agents {
			usage[agent.ID] = &agentWorkloadUsage{}
			if agent.LastSeenAt.After(cutoff) {
				healthyAgents[agent.ID] = agent
			}
		}
		for _, service := range services {
			runtime := serviceRuntime(service.Spec)
			used := usage[service.AllocatedAgentID]
			if used == nil {
				used = &agentWorkloadUsage{}
				usage[service.AllocatedAgentID] = used
			}
			used.services++
			used.cpu += runtime.GetCpuMillis()
			used.memory += runtime.GetMemoryMebibytes()
		}

		moved := false
		for i := range services {
			service := &services[i]
			if _, healthy := healthyAgents[service.AllocatedAgentID]; healthy {
				continue
			}
			allocation, ok := allocations[service.ID]
			if !ok {
				return fmt.Errorf("service %s has no allocation", service.ID)
			}

			var blockedMessage string
			switch {
			case projectKinds[service.ProjectID] == projectKindManaged:
				blockedMessage = "agent unhealthy; managed/trusted workload remains pinned to its trusted agent"
			case serviceVolumeName(service.Spec) != "":
				blockedMessage = fmt.Sprintf("agent unhealthy; service remains pinned because node-bound volume %q requires replicated storage before failover", serviceVolumeName(service.Spec))
			}

			var destination string
			if blockedMessage == "" {
				destination = chooseFailoverDestination(agents, healthyAgents, usage, s.reservedAgentIDs, service)
				if destination == "" {
					blockedMessage = "agent unhealthy; automatic failover blocked because no healthy non-reserved agent has sufficient capacity"
				}
			}

			if blockedMessage != "" {
				changed, err := markAllocationUnavailableForFailover(ctx, tx, service.ID, allocation, blockedMessage, now.UTC())
				if err != nil {
					return err
				}
				if changed {
					result.BlockedServiceIDs = append(result.BlockedServiceIDs, service.ID)
					result.IngressChanged = true
				}
				continue
			}

			oldAgentID := service.AllocatedAgentID
			allocationUpdate, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET agent_id = $1,
				        applied_spec_revision = 0,
				        applied_rollout_generation = 0,
				        phase = 'Pending',
				        message = $2,
				        allocation_ip = '',
				        healthy_ports = $3,
				        healthy = FALSE,
				        updated_at = $4
				  WHERE service_id = $5 AND agent_id = $6`,
				destination,
				fmt.Sprintf("rescheduled from unhealthy agent %s to %s", oldAgentID, destination),
				[]byte("[]"),
				now.UTC(),
				service.ID,
				oldAgentID,
			)
			if err != nil {
				return err
			}
			allocationAffected, err := allocationUpdate.RowsAffected()
			if err != nil {
				return err
			}
			if allocationAffected != 1 {
				return errConcurrentUpdate
			}

			runtime := serviceRuntime(service.Spec)
			if oldUsage := usage[oldAgentID]; oldUsage != nil {
				oldUsage.services--
				oldUsage.cpu -= runtime.GetCpuMillis()
				oldUsage.memory -= runtime.GetMemoryMebibytes()
			}
			newUsage := usage[destination]
			newUsage.services++
			newUsage.cpu += runtime.GetCpuMillis()
			newUsage.memory += runtime.GetMemoryMebibytes()
			service.AllocatedAgentID = destination
			result.MovedServiceIDs = append(result.MovedServiceIDs, service.ID)
			result.IngressChanged = true
			moved = true
		}

		if moved {
			// Allocation host identity is cluster-wide mesh state. Bumping all agents
			// also guarantees a recovered old agent receives removal desired state.
			if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
				return err
			}
			for _, agent := range agents {
				result.NotifyAgentIDs = append(result.NotifyAgentIDs, agent.ID)
			}
		}
		return nil
	})
	if err != nil {
		return serviceFailoverResult{}, err
	}
	return result, nil
}

func projectKindsForFailover(ctx context.Context, q serviceQueryer) (map[string]projectKind, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]projectKind)
	for rows.Next() {
		var id string
		var kind projectKind
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

func allocationStatesForFailover(ctx context.Context, q serviceQueryer) (map[string]allocationFailoverState, error) {
	rows, err := q.QueryContext(ctx, `SELECT service_id, phase, message, allocation_ip, healthy_ports, healthy FROM allocations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]allocationFailoverState)
	for rows.Next() {
		var serviceID string
		var state allocationFailoverState
		if err := rows.Scan(&serviceID, &state.phase, &state.message, &state.allocationIP, &state.healthyPorts, &state.healthy); err != nil {
			return nil, err
		}
		out[serviceID] = state
	}
	return out, rows.Err()
}

func chooseFailoverDestination(agents []agentRecord, healthy map[string]agentRecord, usage map[string]*agentWorkloadUsage, reserved []string, service *serviceRecord) string {
	candidates := make([]agentRecord, 0, len(agents))
	for _, agent := range agents {
		if _, ok := healthy[agent.ID]; !ok || agent.ID == service.AllocatedAgentID || slices.Contains(reserved, agent.ID) {
			continue
		}
		candidates = append(candidates, agent)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := usage[candidates[i].ID], usage[candidates[j].ID]
		if left.services != right.services {
			return left.services < right.services
		}
		return candidates[i].ID < candidates[j].ID
	})
	runtime := serviceRuntime(service.Spec)
	for _, candidate := range candidates {
		used := usage[candidate.ID]
		if candidate.CPUMillisCapacity > 0 && used.cpu+runtime.GetCpuMillis() > candidate.CPUMillisCapacity {
			continue
		}
		if candidate.MemoryMebibytesCapcity > 0 && used.memory+runtime.GetMemoryMebibytes() > candidate.MemoryMebibytesCapcity {
			continue
		}
		return candidate.ID
	}
	return ""
}

func markAllocationUnavailableForFailover(ctx context.Context, q serviceQueryer, serviceID string, current allocationFailoverState, message string, now time.Time) (bool, error) {
	if current.phase == allocationPhaseUnavailable && current.message == message && current.allocationIP == "" && len(current.healthyPorts) == 0 && !current.healthy {
		return false, nil
	}
	_, err := q.ExecContext(ctx,
		`UPDATE allocations
		    SET phase = $1, message = $2, allocation_ip = '', healthy_ports = $3, healthy = FALSE, updated_at = $4
		  WHERE service_id = $5`,
		allocationPhaseUnavailable, message, []byte("[]"), now, serviceID,
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
	interval           time.Duration
	unhealthyThreshold time.Duration
	now                func() time.Time
}

func NewServiceFailoverReconciler(store *Store, notifier failoverNotifier, ingress failoverIngress, interval, unhealthyThreshold time.Duration) *ServiceFailoverReconciler {
	return &ServiceFailoverReconciler{
		store: store, notifier: notifier, ingress: ingress,
		interval: interval, unhealthyThreshold: unhealthyThreshold,
		now: time.Now,
	}
}

func (r *ServiceFailoverReconciler) Reconcile(ctx context.Context) (serviceFailoverResult, error) {
	if r == nil || r.store == nil {
		return serviceFailoverResult{}, nil
	}
	result, err := r.store.failoverUnhealthyServices(ctx, r.now().UTC(), r.unhealthyThreshold)
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
