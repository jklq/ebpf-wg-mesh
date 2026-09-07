package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

const (
	allocationRolloutStarting    = "starting"
	allocationRolloutServing     = "serving"
	allocationRolloutWithdrawing = "withdrawing"
	allocationRolloutDraining    = "draining"
	allocationRolloutLost        = "lost"
)

func validateDesiredReplicaCount(count int32) error {
	if count < defaultDesiredReplicaCount || count > maxDesiredReplicaCount {
		return fmt.Errorf("%w: must be between %d and %d", errInvalidReplicaCount, defaultDesiredReplicaCount, maxDesiredReplicaCount)
	}
	return nil
}

func allocationReady(rec allocationRecord) bool {
	return rec.RolloutState != allocationRolloutWithdrawing && rec.RolloutState != allocationRolloutDraining && rec.Healthy &&
		strings.TrimSpace(rec.AllocationIPv4) != "" && strings.TrimSpace(rec.AllocationIPv6) != "" &&
		rec.AppliedSpecRevision >= rec.DesiredSpecRevision &&
		rec.AppliedRolloutGeneration >= rec.DesiredRolloutGeneration
}

func countReadyAllocations(recs []allocationRecord) int32 {
	var ready int32
	for _, rec := range recs {
		if allocationReady(rec) {
			ready++
		}
	}
	return ready
}

func splitLostAllocations(existing []allocationRecord) (live, lost []allocationRecord) {
	for _, alloc := range existing {
		if alloc.RolloutState == allocationRolloutLost {
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

func (s *Store) setServicePlacementMessageTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE services SET placement_message = $1, updated_at = $2 WHERE id = $3`,
		message, now, serviceID,
	)
	return err
}

func (s *Store) insertAllocationTx(ctx context.Context, tx *sql.Tx, service serviceRecord, agentID string, now time.Time) (allocationRecord, error) {
	alloc := allocationRecord{
		ID:                       mustID(),
		ServiceID:                service.ID,
		ProjectID:                service.ProjectID,
		EnvironmentID:            service.EnvironmentID,
		AgentID:                  agentID,
		DesiredSpecRevision:      service.SpecRevision,
		DesiredRolloutGeneration: service.RolloutGeneration,
		Phase:                    "Pending",
		RolloutState:             allocationRolloutStarting,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	var workloadIPv6Subnet string
	if err := tx.QueryRowContext(ctx, `SELECT workload_ipv6_subnet FROM agents WHERE id = $1`, agentID).Scan(&workloadIPv6Subnet); err != nil {
		return allocationRecord{}, err
	}
	var err error
	alloc.AllocationIPv4, err = s.allocateWorkloadIPv4AddressTx(ctx, tx, agentID)
	if err != nil {
		return allocationRecord{}, err
	}
	alloc.AllocationIPv6, err = privateIPv6(workloadIPv6Subnet, service.EnvironmentID, alloc.ID)
	if err != nil {
		return allocationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocations(
		id, service_id, agent_id, desired_spec_revision, applied_spec_revision,
		desired_rollout_generation, applied_rollout_generation, phase, message,
		allocation_ipv4, allocation_ipv6, healthy_ipv4_ports, healthy_ipv6_ports, healthy, restart_observation_json, operator_restart_nonce,
		rollout_state, drain_started_at, drain_deadline, created_at, updated_at
	) VALUES ($1, $2, $3, $4, 0, $5, 0, 'Pending', '', $6, $7, $8, $8, FALSE, '{}', 0, $9, NULL, NULL, $10, $10)`,
		alloc.ID, alloc.ServiceID, alloc.AgentID, alloc.DesiredSpecRevision, alloc.DesiredRolloutGeneration,
		alloc.AllocationIPv4, alloc.AllocationIPv6, []byte("[]"), alloc.RolloutState, now,
	); err != nil {
		return allocationRecord{}, err
	}
	return alloc, nil
}

func selectAllocationsToRemove(existing []allocationRecord, keep int) (removed, remaining []allocationRecord) {
	if keep < 0 {
		keep = 0
	}
	sorted := append([]allocationRecord(nil), existing...)
	sort.SliceStable(sorted, func(i, j int) bool {
		leftReady, rightReady := allocationReady(sorted[i]), allocationReady(sorted[j])
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

func (s *Store) listAllocationsByServiceID(ctx context.Context, serviceID string) ([]allocationRecord, error) {
	return s.listAllocationsByServiceIDQuerier(ctx, s.db, serviceID, false)
}

func (s *Store) listAllocationsByServiceIDQuerier(ctx context.Context, q serviceQueryer, serviceID string, forUpdate bool) ([]allocationRecord, error) {
	query := allocationSelectSQL + ` WHERE a.service_id = $1 ORDER BY a.id ASC`
	if forUpdate {
		query += ` FOR UPDATE OF a`
	}
	rows, err := q.QueryContext(ctx, query, serviceID)
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

const allocationSelectSQL = `SELECT a.id, a.service_id, e.project_id, s.environment_id, a.agent_id,
		        a.desired_spec_revision, a.applied_spec_revision, a.phase, a.message,
		        a.allocation_ipv4, a.allocation_ipv6, a.healthy, a.updated_at, a.desired_rollout_generation,
		        a.applied_rollout_generation, a.healthy_ipv4_ports, a.healthy_ipv6_ports, a.restart_observation_json,
		        a.operator_restart_nonce, a.created_at, a.rollout_state, a.drain_started_at, a.drain_deadline
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id`

func scanAllocationRow(scanner interface{ Scan(...any) error }) (allocationRecord, error) {
	var (
		rec        allocationRecord
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
		return allocationRecord{}, err
	}
	obs, err := decodeRestartObservation(restartRaw)
	if err != nil {
		return allocationRecord{}, err
	}
	rec.Restart = obs
	return rec, nil
}

func (s *Store) agentIDsForServiceQuerier(ctx context.Context, q serviceQueryer, serviceID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT agent_id FROM allocations WHERE service_id = $1 ORDER BY agent_id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
