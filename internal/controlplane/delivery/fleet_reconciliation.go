package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
)

func (d *Delivery) reconcileDrainingAgent(ctx context.Context, agentID string) ([]string, error) {
	s := d.store
	var notify []string
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
			if service.Deletion != nil {
				continue
			}
			if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
				blocked[fmt.Sprintf("allocation %s stays: volume %q is stored on this node and is not replicated", allocation.ID, volumeName)] = struct{}{}
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
			if !ok || current.ResolvedSpec == nil || strings.TrimSpace(current.ArtifactID) == "" {
				blocked["service has no reusable image snapshot for a rolling replacement"] = struct{}{}
				continue
			}
			detail := fmt.Sprintf("Maintenance drain replacing allocation %s from %s", allocation.ID, agentID)
			if _, err := d.copyDeploymentRolloutTargetTx(ctx, tx, service, current, "", reasonAgentDrain, detail, allocation.ID); err != nil {
				switch {
				case errors.Is(err, ErrRolloutInProgress):
					blocked["waiting for an in-progress rollout to finish"] = struct{}{}
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
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT service_id FROM service_delivery_status WHERE placement_message IS NOT NULL ORDER BY service_id FOR UPDATE`)
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
			if service.Deletion != nil {
				continue
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

type nodeLossReplacementResult struct {
	Replaced bool
	Blocked  bool
	Changed  bool
	Bumped   bool
}

func (d *Delivery) failoverServicesFromAgent(ctx context.Context, agentID string, cutoff time.Time) ([]string, []string, error) {
	result, err := d.failoverAgent(ctx, agentID, cutoff, d.live.currentTime())
	return result.NotifyAgentIDs, result.EnvironmentIDs, err
}

func (d *Delivery) failoverAgent(ctx context.Context, agentID string, cutoff, now time.Time) (ServiceFailoverResult, error) {
	// Probe outside the transaction, then validate the assignment generation
	// before using the evidence. A management outage alone cannot evict a
	// workload that is still accepting traffic.
	agent, ok := d.live.Agent(agentID)
	if !ok || agent.LifecycleState != AgentStateUnavailable || agent.LastSeenAt.After(cutoff) {
		return ServiceFailoverResult{}, nil
	}
	retained := d.allocationsRetainedByTraffic(ctx, agentID)
	if err := ctx.Err(); err != nil {
		return ServiceFailoverResult{}, err
	}

	var result ServiceFailoverResult
	changedEnvironments := make(map[string]struct{})
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if d.live == nil || !d.live.Serving() {
			return ErrNotLiveOwner
		}
		agent, ok := d.live.Agent(agentID)
		if !ok || agent.LifecycleState != AgentStateUnavailable || agent.StateBeforeUnavailable != AgentStateActive || agent.LastSeenAt.After(cutoff) {
			return nil
		}
		allocations, err := listAllocationsForFailover(ctx, d.store, tx, agentID)
		if err != nil {
			return err
		}
		for _, allocation := range allocations {
			if evidence, ok := retained[allocation.ID]; ok &&
				evidence.DesiredSpecRevision == allocation.DesiredSpecRevision &&
				evidence.DesiredRolloutGeneration == allocation.DesiredRolloutGeneration &&
				evidence.AllocationIPv4 == allocation.AllocationIPv4 && evidence.AllocationIPv6 == allocation.AllocationIPv6 {
				continue
			}

			replacement, err := d.replaceLostNodeAllocationTx(ctx, tx, agentID, allocation, now)
			if err != nil {
				return err
			}
			if !replacement.Changed {
				continue
			}
			if replacement.Blocked {
				result.BlockedServiceIDs = append(result.BlockedServiceIDs, allocation.ServiceID)
			} else if replacement.Replaced {
				result.MovedServiceIDs = append(result.MovedServiceIDs, allocation.ServiceID)
			}
			result.IngressChanged = true
			changedEnvironments[allocation.EnvironmentID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return ServiceFailoverResult{}, err
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

func (d *Delivery) replaceLostNodeAllocationTx(ctx context.Context, tx *sql.Tx, deadAgentID string, allocation AllocationRecord, now time.Time) (nodeLossReplacementResult, error) {
	s := d.store
	var result nodeLossReplacementResult
	switch lostAllocationDisposition(allocation, deadAgentID) {
	case failoverIgnore:
		return result, nil
	case failoverFinishDrain:
		if err := d.applyAllocationMutationsTx(ctx, tx, now, allocationMutation{
			Kind: mutationMarkLost, AllocationID: allocation.ID, Message: "node lost while draining; allocation will be removed",
		}); err != nil {
			return result, err
		}
		return nodeLossReplacementResult{Changed: true, Replaced: true}, nil
	}

	service, err := s.serviceByIDInternalQuerier(ctx, tx, allocation.ServiceID)
	if err != nil {
		return result, err
	}
	if service.Deletion != nil {
		return result, nil
	}
	pendingChanges, err := s.serviceHasUnappliedChangesQuerier(ctx, tx, service)
	if err != nil {
		return result, err
	}
	if pendingChanges {
		return result, nil
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
	snapshot.ReusableImage = ok && current.ResolvedSpec != nil && strings.TrimSpace(current.ArtifactID) != ""
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
		changed, err := d.store.setFailoverMessageTx(ctx, tx, allocation.ID, decision.Message, now)
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

	if err := d.applyAllocationMutationsTx(ctx, tx, now, allocationMutation{
		Kind: mutationMarkLost, AllocationID: allocation.ID, Message: lostMessage,
	}); err != nil {
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

type ServiceFailoverResult struct {
	MovedServiceIDs   []string
	BlockedServiceIDs []string
	NotifyAgentIDs    []string
	EnvironmentIDs    []string
	IngressChanged    bool
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
	query := allocationSelectSQL + ` JOIN projects p ON p.id = e.project_id`
	filters := []string{`s.deleted_at IS NULL`, `e.deleted_at IS NULL`, `p.deleted_at IS NULL`}
	args := []any{}
	if agentID != "" {
		filters = append([]string{`a.agent_id = $1`}, filters...)
		args = append(args, agentID)
	}
	query += ` WHERE ` + strings.Join(filters, ` AND `)
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

func (s *persistence) setFailoverMessageTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) (bool, error) {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT intent_message FROM allocation_assignments WHERE id = $1`, allocationID).Scan(&current); err != nil {
		return false, err
	}
	if current == message {
		return false, nil
	}
	err := (&Delivery{store: s}).applyAllocationMutationsTx(ctx, tx, now, allocationMutation{
		Kind: mutationSetMessage, AllocationID: allocationID, Message: message,
	})
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

type failoverAction uint8

const (
	failoverIgnore failoverAction = iota
	failoverFinishDrain
	failoverBlocked
	failoverAdvance
	failoverReplace
)

type failoverSnapshot struct {
	Allocation       AllocationRecord
	DeadAgentID      string
	ProjectKind      ProjectKind
	VolumeName       string
	PlacementFailure string
	ReusableImage    bool
	RolloutState     string
	Generation       int64
	// PendingChanges is true when the service has an acknowledged but not yet
	// released spec change. Copying the current deployment would roll that
	// change back, so failover leaves the allocation to the release path.
	PendingChanges bool
}

type failoverDecision struct {
	Action  failoverAction
	Message string
}

func lostAllocationDisposition(allocation AllocationRecord, deadAgentID string) failoverAction {
	if allocation.AgentID != deadAgentID || allocation.RolloutState == AllocationRolloutLost {
		return failoverIgnore
	}
	if allocation.RolloutState == AllocationRolloutDraining || allocation.RolloutState == AllocationRolloutWithdrawing {
		return failoverFinishDrain
	}
	return failoverReplace
}

func failoverPinnedMessage(kind ProjectKind, volumeName string) string {
	if kind == ProjectKindManaged {
		return "agent unhealthy; managed/trusted workload remains pinned to its trusted agent"
	}
	if volumeName != "" {
		return fmt.Sprintf("agent unhealthy; volume %q is stored on that node and is not replicated, so the service waits for the node instead of starting elsewhere with an empty volume", volumeName)
	}
	return ""
}

func failoverPlacementMessage(detail string) string {
	if detail == "" {
		detail = "no healthy non-reserved agent has sufficient capacity"
	}
	return "agent unhealthy; automatic failover blocked because " + detail
}

func decideFailover(snapshot failoverSnapshot) failoverDecision {
	if action := lostAllocationDisposition(snapshot.Allocation, snapshot.DeadAgentID); action != failoverReplace {
		return failoverDecision{Action: action}
	}
	// A staged update supersedes the running deployment. Leave it entirely to
	// the release path: failover must not probe placement or mutate intent.
	if snapshot.PendingChanges {
		return failoverDecision{Action: failoverIgnore}
	}
	if message := failoverPinnedMessage(snapshot.ProjectKind, snapshot.VolumeName); message != "" {
		return failoverDecision{Action: failoverBlocked, Message: message}
	}
	if snapshot.PlacementFailure != "" {
		return failoverDecision{Action: failoverBlocked, Message: failoverPlacementMessage(snapshot.PlacementFailure)}
	}
	if !snapshot.ReusableImage {
		return failoverDecision{Action: failoverBlocked, Message: "agent unhealthy; service has no reusable image snapshot for a rolling replacement"}
	}
	if snapshot.RolloutState == rolloutStateInProgress || snapshot.RolloutState == rolloutStatePendingBuild {
		if snapshot.Allocation.DesiredRolloutGeneration < snapshot.Generation {
			return failoverDecision{Action: failoverIgnore}
		}
		return failoverDecision{Action: failoverAdvance}
	}
	return failoverDecision{Action: failoverReplace}
}
