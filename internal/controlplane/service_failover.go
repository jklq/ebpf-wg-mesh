package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"ebof-wg-mesh/internal/restartpolicy"
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
		servicesByID := make(map[string]*serviceRecord, len(services))
		occupiedByService := make(map[string]map[string]struct{}, len(services))
		for i := range services {
			servicesByID[services[i].ID] = &services[i]
			occupiedByService[services[i].ID] = map[string]struct{}{}
		}
		for _, allocation := range allocations {
			service := servicesByID[allocation.ServiceID]
			if service == nil {
				continue
			}
			runtime := serviceRuntime(service.Spec)
			used := usage[allocation.AgentID]
			if used == nil {
				used = &agentWorkloadUsage{}
				usage[allocation.AgentID] = used
			}
			used.services++
			used.cpu += runtime.GetCpuMillis()
			used.memory += runtime.GetMemoryMebibytes()
			occupiedByService[allocation.ServiceID][allocation.AgentID] = struct{}{}
		}

		moved := false
		for i := range allocations {
			allocation := allocations[i]
			service := servicesByID[allocation.ServiceID]
			if service == nil {
				return fmt.Errorf("allocation %s has no service", allocation.ID)
			}
			if _, healthy := healthyAgents[allocation.AgentID]; healthy {
				continue
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
				destination = chooseFailoverDestination(agents, healthyAgents, usage, s.reservedAgentIDs, service, allocation.AgentID, occupiedByService[service.ID])
				if destination == "" {
					blockedMessage = "agent unhealthy; automatic failover blocked because no healthy non-reserved agent has sufficient capacity"
				}
			}

			if blockedMessage != "" {
				changed, err := markAllocationUnavailableForFailover(ctx, tx, allocation.ID, allocationFailoverState{
					phase: allocation.Phase, message: allocation.Message, allocationIP: allocation.AllocationIP,
					healthyPorts: allocation.HealthyPorts, healthy: allocation.Healthy,
				}, blockedMessage, now.UTC())
				if err != nil {
					return err
				}
				if changed {
					result.BlockedServiceIDs = append(result.BlockedServiceIDs, service.ID)
					result.IngressChanged = true
				}
				continue
			}

			oldAgentID := allocation.AgentID
			nodeLoss, err := encodeRestartObservation(restartpolicy.NodeLossObservation(now.UTC(), 0, 0))
			if err != nil {
				return err
			}
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
				        restart_observation_json = $4,
				        updated_at = $5
				  WHERE id = $6 AND agent_id = $7`,
				destination,
				fmt.Sprintf("rescheduled from unhealthy agent %s to %s", oldAgentID, destination),
				[]byte("[]"),
				nodeLoss,
				now.UTC(),
				allocation.ID,
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
			delete(occupiedByService[service.ID], oldAgentID)
			occupiedByService[service.ID][destination] = struct{}{}
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

func chooseFailoverDestination(agents []agentRecord, healthy map[string]agentRecord, usage map[string]*agentWorkloadUsage, reserved []string, service *serviceRecord, currentAgentID string, occupied map[string]struct{}) string {
	if dest := firstFailoverCandidate(agents, healthy, usage, reserved, service, currentAgentID, occupied, true); dest != "" {
		return dest
	}
	return firstFailoverCandidate(agents, healthy, usage, reserved, service, currentAgentID, occupied, false)
}

func firstFailoverCandidate(agents []agentRecord, healthy map[string]agentRecord, usage map[string]*agentWorkloadUsage, reserved []string, service *serviceRecord, currentAgentID string, occupied map[string]struct{}, avoidOccupied bool) string {
	candidates := make([]agentRecord, 0, len(agents))
	for _, agent := range agents {
		if _, ok := healthy[agent.ID]; !ok || agent.ID == currentAgentID || slices.Contains(reserved, agent.ID) {
			continue
		}
		if avoidOccupied {
			if _, taken := occupied[agent.ID]; taken {
				continue
			}
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
