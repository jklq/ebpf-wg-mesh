package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"

	"github.com/google/uuid"
)

type allocationMutationKind string

const (
	mutationCreateAllocation allocationMutationKind = "create_allocation"
	mutationChangeIntent     allocationMutationKind = "change_allocation_intent"
	mutationSetMessage       allocationMutationKind = "set_allocation_message"
	mutationRetarget         allocationMutationKind = "retarget_allocation"
	mutationReserveAddress   allocationMutationKind = "reserve_allocation_address"
	mutationBeginDrain       allocationMutationKind = "begin_drain"
	mutationCompleteDrain    allocationMutationKind = "complete_drain"
	mutationMarkLost         allocationMutationKind = "mark_allocation_lost"
)

type allocationMutation struct {
	Kind         allocationMutationKind
	Allocation   AllocationAssignment
	AllocationID string
	State        allocationAssignmentState
	Message      string
}

func (m allocationMutation) validate(index int) error {
	switch m.Kind {
	case mutationCreateAllocation:
		if m.Allocation.ID == "" || m.Allocation.AgentID == "" || m.Allocation.IPv4 == "" || m.Allocation.IPv6 == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete allocation reservation", index)
		}
	case mutationChangeIntent, mutationBeginDrain:
		if m.State.AllocationID == "" {
			return fmt.Errorf("allocation mutation %d has no allocation", index)
		}
	case mutationCompleteDrain, mutationMarkLost, mutationSetMessage:
		if m.AllocationID == "" {
			return fmt.Errorf("allocation mutation %d has no allocation", index)
		}
	case mutationReserveAddress:
		if m.AllocationID == "" || m.Allocation.IPv4 == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete address reservation", index)
		}
	case mutationRetarget:
		if m.Allocation.ID == "" || m.Allocation.DeploymentID == "" {
			return fmt.Errorf("allocation mutation %d has an incomplete retarget", index)
		}
	default:
		return fmt.Errorf("allocation mutation %d has unknown kind %q", index, m.Kind)
	}
	return nil
}

func (d *Delivery) applyAllocationMutationsTx(ctx context.Context, tx *sql.Tx, now time.Time, mutations ...allocationMutation) error {
	if now.IsZero() {
		return fmt.Errorf("allocation mutations have no timestamp")
	}
	now = now.UTC()
	for i, mutation := range mutations {
		if err := mutation.validate(i); err != nil {
			return err
		}
	}
	for _, mutation := range mutations {
		switch mutation.Kind {
		case mutationCreateAllocation:
			if err := d.store.insertAllocationAssignmentTx(ctx, tx, mutation.Allocation); err != nil {
				return err
			}
		case mutationChangeIntent, mutationBeginDrain:
			if err := d.store.setAllocationStateTx(ctx, tx, mutation.State); err != nil {
				return err
			}
		case mutationCompleteDrain:
			if err := d.store.deleteAllocationAssignmentTx(ctx, tx, mutation.AllocationID); err != nil {
				return err
			}
		case mutationMarkLost:
			if err := d.store.markAssignmentLostTx(ctx, tx, mutation.AllocationID, mutation.Message, now); err != nil {
				return err
			}
		case mutationSetMessage:
			if err := d.store.setAssignmentMessageTx(ctx, tx, mutation.AllocationID, mutation.Message, now); err != nil {
				return err
			}
		case mutationRetarget:
			assignment := mutation.Allocation
			if _, err := journal.AssignmentRow(assignment.ID).Exec(ctx, tx, `UPDATE allocation_assignments
				SET deployment_id = $2, desired_spec_revision = $3, desired_rollout_generation = $4,
				    intent = $5, intent_message = '', updated_at = $6 WHERE id = $1`,
				assignment.ID, assignment.DeploymentID, assignment.SpecRevision, assignment.RolloutGeneration,
				assignment.Intent, now); err != nil {
				return err
			}
		case mutationReserveAddress:
			if _, err := journal.AssignmentRow(mutation.AllocationID).Exec(ctx, tx, `UPDATE allocation_assignments
				SET allocation_ipv4 = $2, updated_at = $3 WHERE id = $1`,
				mutation.AllocationID, mutation.Allocation.IPv4, now); err != nil {
				return err
			}
		default:
			return fmt.Errorf("allocation mutation has unknown kind %q", mutation.Kind)
		}
	}
	return nil
}

func (d *Delivery) deleteStartingAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID string, generation *int64, now time.Time) error {
	query := `SELECT id FROM allocation_assignments WHERE service_id = $1 AND rollout_state = $2`
	args := []any{serviceID, AllocationRolloutStarting}
	if generation != nil {
		query += ` AND desired_rollout_generation = $3`
		args = append(args, *generation)
	}
	query += ` ORDER BY id FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	var mutations []allocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		mutations = append(mutations, allocationMutation{Kind: mutationCompleteDrain, AllocationID: id})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}

func (d *Delivery) withdrawServiceAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND rollout_state NOT IN ($2, $3, $4) ORDER BY id FOR UPDATE`,
		serviceID, AllocationRolloutWithdrawing, AllocationRolloutDraining, AllocationRolloutLost)
	if err != nil {
		return 0, err
	}
	var mutations []allocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		mutations = append(mutations, allocationMutation{Kind: mutationChangeIntent, State: allocationAssignmentState{
			AllocationID: id, RolloutState: AllocationRolloutWithdrawing, Intent: allocationIntentRun,
			IntentMessage: message, UpdatedAt: now,
		}})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return int64(len(mutations)), d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}

func (d *Delivery) retargetAllocationsTx(ctx context.Context, tx *sql.Tx, serviceID, agentID, deploymentID string, specRevision, generation int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE service_id = $1 AND agent_id = $2 AND rollout_state IN ($3, $4) ORDER BY id FOR UPDATE`,
		serviceID, agentID, AllocationRolloutStarting, AllocationRolloutServing)
	if err != nil {
		return err
	}
	var mutations []allocationMutation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		mutations = append(mutations, allocationMutation{Kind: mutationRetarget, Allocation: AllocationAssignment{
			ID: id, DeploymentID: deploymentID, SpecRevision: specRevision, RolloutGeneration: generation, Intent: allocationIntentRun,
		}})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return d.applyAllocationMutationsTx(ctx, tx, now, mutations...)
}

const (
	AllocationRolloutStarting    = "starting"
	AllocationRolloutServing     = "serving"
	AllocationRolloutWithdrawing = "withdrawing"
	AllocationRolloutDraining    = "draining"
	AllocationRolloutLost        = "lost"
)

func validateDesiredReplicaCount(count int32) error {
	if count < DefaultDesiredReplicaCount || count > MaxDesiredReplicaCount {
		return fmt.Errorf("%w: must be between %d and %d", ErrInvalidReplicaCount, DefaultDesiredReplicaCount, MaxDesiredReplicaCount)
	}
	return nil
}

func AllocationReady(rec AllocationRecord) bool {
	return rec.RolloutState != AllocationRolloutWithdrawing && rec.RolloutState != AllocationRolloutDraining && rec.Healthy &&
		strings.TrimSpace(rec.AllocationIPv4) != "" && strings.TrimSpace(rec.AllocationIPv6) != "" &&
		rec.AppliedSpecRevision >= rec.DesiredSpecRevision &&
		rec.AppliedRolloutGeneration >= rec.DesiredRolloutGeneration
}

func splitLostAllocations(existing []AllocationRecord) (live, lost []AllocationRecord) {
	for _, alloc := range existing {
		if alloc.RolloutState == AllocationRolloutLost {
			lost = append(lost, alloc)
			continue
		}
		live = append(live, alloc)
	}
	return live, lost
}

func pendingPlacementMessage(placed, desired int, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "no active healthy node satisfies the placement constraints"
	}
	return fmt.Sprintf("%d of %d replicas placed; %s", placed, desired, reason)
}

func (s *persistence) setServicePlacementMessageTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) error {
	result, err := journal.ServiceRow(serviceID).Exec(ctx, tx,
		`UPDATE service_delivery_status
		 SET placement_message = NULLIF($1, ''), updated_at = $2
		 WHERE service_id = $3
		   AND placement_message IS DISTINCT FROM NULLIF($1, '')`,
		message, now, serviceID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return nil
	}

	return nil
}

func (d *Delivery) planAllocationCreationTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, agentID string, now time.Time) (AllocationRecord, allocationMutation, error) {
	s := d.store
	alloc := AllocationRecord{
		ID:                       uuid.NewString(),
		ServiceID:                service.ID,
		ProjectID:                service.ProjectID,
		EnvironmentID:            service.EnvironmentID,
		AgentID:                  agentID,
		DesiredSpecRevision:      service.SpecRevision,
		DesiredRolloutGeneration: service.RolloutGeneration,
		Phase:                    "Pending",
		RolloutState:             AllocationRolloutStarting,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	var workloadIPv6Subnet string
	if err := tx.QueryRowContext(ctx, `SELECT workload_ipv6_subnet FROM agents WHERE id = $1`, agentID).Scan(&workloadIPv6Subnet); err != nil {
		return AllocationRecord{}, allocationMutation{}, err
	}
	var err error
	alloc.AllocationIPv4, err = s.allocateWorkloadIPv4AddressTx(ctx, tx, agentID)
	if err != nil {
		return AllocationRecord{}, allocationMutation{}, err
	}
	alloc.AllocationIPv6, err = privateIPv6(workloadIPv6Subnet, service.EnvironmentID, alloc.ID)
	if err != nil {
		return AllocationRecord{}, allocationMutation{}, err
	}
	var deploymentID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM deployments
		WHERE service_id = $1 AND rollout_generation = $2
		ORDER BY is_current DESC, created_at DESC, id DESC LIMIT 1`, service.ID, service.RolloutGeneration).Scan(&deploymentID); err != nil {
		return AllocationRecord{}, allocationMutation{}, fmt.Errorf("load allocation deployment: %w", err)
	}
	assignment := AllocationAssignment{
		ID: alloc.ID, ServiceID: alloc.ServiceID, DeploymentID: deploymentID, AgentID: alloc.AgentID,
		SpecRevision: alloc.DesiredSpecRevision, RolloutGeneration: alloc.DesiredRolloutGeneration,
		IPv4: alloc.AllocationIPv4, IPv6: alloc.AllocationIPv6,
		RolloutState: alloc.RolloutState, Intent: allocationIntentRun,
		CreatedAt: now, UpdatedAt: now,
	}
	return alloc, allocationMutation{Kind: mutationCreateAllocation, Allocation: assignment}, nil
}

func (d *Delivery) insertAllocationTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, agentID string, now time.Time) (AllocationRecord, error) {
	alloc, mutation, err := d.planAllocationCreationTx(ctx, tx, service, agentID, now)
	if err != nil {
		return AllocationRecord{}, err
	}
	if err := d.applyAllocationMutationsTx(ctx, tx, now, mutation); err != nil {
		return AllocationRecord{}, err
	}
	return alloc, nil
}

func selectAllocationsToRemove(existing []AllocationRecord, keep int) (removed, remaining []AllocationRecord) {
	if keep < 0 {
		keep = 0
	}
	sorted := append([]AllocationRecord(nil), existing...)
	sort.SliceStable(sorted, func(i, j int) bool {
		leftReady, rightReady := AllocationReady(sorted[i]), AllocationReady(sorted[j])
		if leftReady != rightReady {
			return leftReady
		}
		if sorted[i].Healthy != sorted[j].Healthy {
			return sorted[i].Healthy
		}
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	if keep >= len(sorted) {
		return nil, sorted
	}
	return sorted[keep:], sorted[:keep]
}

func firstEligibleReplicaAgent(candidates []placementCandidate, spec *platformv1.ServiceSpec, reserved []string, occupied map[string]struct{}, avoidOccupied, avoidFailureDomain bool) string {
	occupiedDomains := make(map[string]struct{}, len(occupied))
	for _, candidate := range candidates {
		if _, ok := occupied[candidate.ID]; ok {
			occupiedDomains[candidateFailureDomain(candidate)] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		if slices.Contains(reserved, candidate.ID) {
			continue
		}
		if avoidOccupied {
			if _, taken := occupied[candidate.ID]; taken {
				continue
			}
		}
		if avoidFailureDomain {
			if _, taken := occupiedDomains[candidateFailureDomain(candidate)]; taken {
				continue
			}
		}
		if !candidateEligible(candidate, spec) {
			continue
		}
		return candidate.ID
	}
	return ""
}

func candidateFailureDomain(candidate placementCandidate) string {
	if candidate.FailureDomain != "" {
		return candidate.FailureDomain
	}
	if candidate.Zone != "" {
		return candidate.Region + "/" + candidate.Zone
	}
	return candidate.ID
}

func candidateEligible(candidate placementCandidate, spec *platformv1.ServiceSpec) bool {
	if spec != nil && strings.TrimSpace(spec.GetPlacementRegion()) != "" && candidate.Region != strings.TrimSpace(spec.GetPlacementRegion()) {
		return false
	}
	for _, required := range []string{"containerd", "wireguard", "ebpf-policy"} {
		if !slices.Contains(candidate.RuntimeCapabilities, required) {
			return false
		}
	}
	return candidateHasCapacity(candidate, spec)
}

func placementFailureReason(candidates []placementCandidate, spec *platformv1.ServiceSpec, reserved []string) string {
	region := strings.TrimSpace(spec.GetPlacementRegion())
	regionMatches, capable, cpuOK, memoryOK := 0, 0, 0, 0
	for _, candidate := range candidates {
		if slices.Contains(reserved, candidate.ID) {
			continue
		}
		if region != "" && candidate.Region != region {
			continue
		}
		regionMatches++
		if !slices.Contains(candidate.RuntimeCapabilities, "containerd") || !slices.Contains(candidate.RuntimeCapabilities, "wireguard") || !slices.Contains(candidate.RuntimeCapabilities, "ebpf-policy") {
			continue
		}
		capable++
		runtime := serviceRuntime(spec)
		if candidate.CPUMillisCapacity <= 0 || candidate.UsedCPUMillis+runtime.GetCpuMillis() <= candidate.CPUMillisCapacity {
			cpuOK++
		}
		if candidate.MemoryMebibytesCapcity <= 0 || candidate.UsedMemoryMebibytes+runtime.GetMemoryMebibytes() <= candidate.MemoryMebibytesCapcity {
			memoryOK++
		}
	}
	switch {
	case len(candidates) == 0:
		return "no active healthy node is schedulable (enrolling, cordoned, draining, unavailable, and retired nodes are excluded)"
	case region != "" && regionMatches == 0:
		return fmt.Sprintf("no active healthy node is available in required region %q", region)
	case capable == 0:
		return "active nodes do not report the required containerd, WireGuard, and eBPF policy capabilities"
	case cpuOK == 0:
		return "insufficient schedulable CPU after operator reservations"
	case memoryOK == 0:
		return "insufficient schedulable memory after operator reservations"
	default:
		return "insufficient alternate capacity after operator reservations"
	}
}

func candidateHasCapacity(candidate placementCandidate, spec *platformv1.ServiceSpec) bool {
	if spec == nil {
		return true
	}
	runtime := serviceRuntime(spec)
	if candidate.CPUMillisCapacity > 0 && candidate.UsedCPUMillis+runtime.GetCpuMillis() > candidate.CPUMillisCapacity {
		return false
	}
	if candidate.MemoryMebibytesCapcity > 0 && candidate.UsedMemoryMebibytes+runtime.GetMemoryMebibytes() > candidate.MemoryMebibytesCapcity {
		return false
	}
	return true
}

func (s *persistence) listAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	_ = ctx
	if s != nil && s.live != nil {
		return s.live.AllocationsByServiceIfServing(serviceID)
	}
	return s.listAllocationsByServiceIDQuerier(ctx, s.db, serviceID, false)
}

func (s *persistence) listAllocationsByServiceIDQuerier(ctx context.Context, q ServiceQueryer, serviceID string, forUpdate bool) ([]AllocationRecord, error) {
	if forUpdate {
		locked, err := q.QueryContext(ctx, `SELECT id FROM allocation_assignments WHERE service_id = $1 ORDER BY id FOR UPDATE`, serviceID)
		if err != nil {
			return nil, err
		}
		for locked.Next() {
			var id string
			if err := locked.Scan(&id); err != nil {
				locked.Close()
				return nil, err
			}
		}
		if err := locked.Close(); err != nil {
			return nil, err
		}
	}
	query := allocationSelectSQL + ` WHERE a.service_id = $1 ORDER BY a.id ASC`
	rows, err := q.QueryContext(ctx, query, serviceID)
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

const allocationSelectSQL = `SELECT a.id, a.service_id, e.project_id, s.environment_id, a.agent_id,
		        a.desired_spec_revision, a.applied_spec_revision, a.phase, a.message,
		        a.allocation_ipv4, a.allocation_ipv6, a.healthy, a.updated_at, a.desired_rollout_generation,
		        a.applied_rollout_generation, a.healthy_ipv4_ports, a.healthy_ipv6_ports, a.restart_observation_json,
		        a.operator_restart_nonce, a.created_at, a.rollout_state, a.drain_started_at, a.drain_deadline
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id`

func scanAllocationRow(scanner interface{ Scan(...any) error }) (AllocationRecord, error) {
	var (
		rec        AllocationRecord
		restartRaw []byte
	)
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.ProjectID,
		&rec.EnvironmentID,
		&rec.AgentID,
		&rec.DesiredSpecRevision,
		&rec.AppliedSpecRevision,
		&rec.Phase,
		&rec.Message,
		&rec.AllocationIPv4,
		&rec.AllocationIPv6,
		&rec.Healthy,
		&rec.UpdatedAt,
		&rec.DesiredRolloutGeneration,
		&rec.AppliedRolloutGeneration,
		(*jsonInt32Slice)(&rec.HealthyIPv4Ports),
		(*jsonInt32Slice)(&rec.HealthyIPv6Ports),
		&restartRaw,
		&rec.OperatorRestartNonce,
		&rec.CreatedAt,
		&rec.RolloutState,
		&rec.DrainStartedAt,
		&rec.DrainDeadline,
	); err != nil {
		return AllocationRecord{}, err
	}
	obs, err := decodeRestartObservation(restartRaw)
	if err != nil {
		return AllocationRecord{}, err
	}
	rec.Restart = obs
	return rec, nil
}

const (
	allocationIntentRun   = "run"
	allocationIntentDrain = "drain"
)

type allocationAssignmentState struct {
	AllocationID  string
	RolloutState  string
	Intent        string
	IntentMessage string
	DrainStarted  sql.NullTime
	DrainDeadline sql.NullTime
	UpdatedAt     time.Time
}

func (s *persistence) setAllocationStateTx(ctx context.Context, tx *sql.Tx, state allocationAssignmentState) error {
	if _, err := journal.AssignmentRow(state.AllocationID).Exec(ctx, tx, `UPDATE allocation_assignments
		SET rollout_state = $2, intent = $3, intent_message = $4,
		    drain_started_at = $5, drain_deadline = $6, updated_at = $7
		WHERE id = $1`, state.AllocationID, state.RolloutState, state.Intent, state.IntentMessage,
		state.DrainStarted, state.DrainDeadline, state.UpdatedAt); err != nil {
		return err
	}
	return nil
}

func (s *persistence) insertAllocationAssignmentTx(ctx context.Context, tx *sql.Tx, assignment AllocationAssignment) error {
	if _, err := journal.AssignmentRow(assignment.ID).Exec(ctx, tx, `INSERT INTO allocation_assignments(
		id, service_id, deployment_id, agent_id, desired_spec_revision,
		desired_rollout_generation, allocation_ipv4, allocation_ipv6,
		operator_restart_nonce, rollout_state, intent, intent_message,
		drain_started_at, drain_deadline, created_at, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		assignment.ID, assignment.ServiceID, assignment.DeploymentID, assignment.AgentID,
		assignment.SpecRevision, assignment.RolloutGeneration, assignment.IPv4, assignment.IPv6,
		assignment.OperatorRestartNonce, assignment.RolloutState, assignment.Intent, assignment.IntentMessage,
		assignment.DrainStartedAt, assignment.DrainDeadline, assignment.CreatedAt, assignment.UpdatedAt); err != nil {
		return err
	}
	return nil
}

func (s *persistence) deleteAllocationAssignmentTx(ctx context.Context, tx *sql.Tx, allocationID string) error {
	if _, err := journal.AssignmentRow(allocationID).Exec(ctx, tx, `DELETE FROM allocation_assignments WHERE id = $1`, allocationID); err != nil {
		return err
	}
	return nil
}

func (s *persistence) markAssignmentLostTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	if _, err := journal.AssignmentRow(allocationID).Exec(ctx, tx, `UPDATE allocation_assignments
		SET rollout_state = $2, intent = $3, intent_message = $4,
		    allocation_ipv4 = '', allocation_ipv6 = '', updated_at = $5
		WHERE id = $1`, allocationID, AllocationRolloutLost, allocationIntentRun, message, now); err != nil {
		return err
	}
	return nil
}

func (s *persistence) setAssignmentMessageTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	if _, err := journal.AssignmentRow(allocationID).Exec(ctx, tx, `UPDATE allocation_assignments SET intent_message = $2, updated_at = $3 WHERE id = $1`, allocationID, message, now); err != nil {
		return err
	}
	return nil
}
