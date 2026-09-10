package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/google/uuid"
	"slices"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

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
	_, err := tx.ExecContext(ctx,
		`UPDATE services SET placement_message = $1, updated_at = $2 WHERE id = $3`,
		message, now, serviceID,
	)
	return err
}

func (d *Delivery) planAllocationCreationTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, agentID string, now time.Time) (AllocationRecord, SchedulingDecision, error) {
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
		return AllocationRecord{}, SchedulingDecision{}, err
	}
	var err error
	alloc.AllocationIPv4, err = s.allocateWorkloadIPv4AddressTx(ctx, tx, agentID)
	if err != nil {
		return AllocationRecord{}, SchedulingDecision{}, err
	}
	alloc.AllocationIPv6, err = privateIPv6(workloadIPv6Subnet, service.EnvironmentID, alloc.ID)
	if err != nil {
		return AllocationRecord{}, SchedulingDecision{}, err
	}
	var deploymentID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM deployments
		WHERE service_id = $1 AND rollout_generation = $2
		ORDER BY is_current DESC, created_at DESC, id DESC LIMIT 1`, service.ID, service.RolloutGeneration).Scan(&deploymentID); err != nil {
		return AllocationRecord{}, SchedulingDecision{}, fmt.Errorf("load allocation deployment: %w", err)
	}
	assignment := AllocationAssignment{
		ID: alloc.ID, ServiceID: alloc.ServiceID, DeploymentID: deploymentID, AgentID: alloc.AgentID,
		SpecRevision: alloc.DesiredSpecRevision, RolloutGeneration: alloc.DesiredRolloutGeneration,
		IPv4: alloc.AllocationIPv4, IPv6: alloc.AllocationIPv6,
		RolloutState: alloc.RolloutState, Intent: allocationIntentRun,
		CreatedAt: now, UpdatedAt: now,
	}
	return alloc, SchedulingDecision{Kind: DecisionCreateAllocation, Allocation: assignment}, nil
}

func (d *Delivery) insertAllocationTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, agentID string, now time.Time) (AllocationRecord, error) {
	alloc, decision, err := d.planAllocationCreationTx(ctx, tx, service, agentID, now)
	if err != nil {
		return AllocationRecord{}, err
	}
	if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, decision)); err != nil {
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
