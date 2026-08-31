package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
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
		strings.TrimSpace(rec.AllocationIP) != "" &&
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

func (s *Store) scaleService(ctx context.Context, userID, projectID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error) {
	var (
		current     serviceRecord
		allocations []allocationRecord
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, allocations, err = s.scaleServiceTx(ctx, tx, userID, projectID, serviceID, desired)
		return err
	})
	if err != nil {
		return serviceRecord{}, nil, err
	}
	current, err = s.serviceByID(ctx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	allocations, err = s.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	return current, allocations, nil
}

func (s *Store) scaleServiceTx(ctx context.Context, tx *sql.Tx, userID, projectID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error) {
	if err := validateDesiredReplicaCount(desired); err != nil {
		return serviceRecord{}, nil, err
	}
	current, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
		return serviceRecord{}, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 FOR UPDATE`, serviceID).Scan(&serviceID); err != nil {
		return serviceRecord{}, nil, err
	}
	if err := validateVolumeReplicaCompatibility(current.Spec, desired); err != nil {
		return serviceRecord{}, nil, err
	}

	nextSpec := current.Spec
	if nextSpec == nil {
		nextSpec = &platformv1.ServiceSpec{}
	} else {
		nextSpec = proto.Clone(nextSpec).(*platformv1.ServiceSpec)
	}
	nextSpec.DesiredReplicaCount = replicaCountPtr(desired)
	updated, _, _, err := s.updateServiceTx(ctx, tx, userID, projectID, serviceID, current.Name, nextSpec)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, false)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	return updated, allocations, nil
}

func (s *Store) reconcileServiceReplicasTx(ctx context.Context, tx *sql.Tx, service serviceRecord, preferredAgentID string, now time.Time) ([]allocationRecord, error) {
	desired := service.DesiredReplicaCount
	if err := validateVolumeReplicaCompatibility(service.Spec, desired); err != nil {
		return nil, err
	}
	if desired <= 0 && service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return nil, nil
	}
	existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	if service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return existing, nil
	}

	live, lost := splitLostAllocations(existing)
	existing = live

	if int32(len(existing)) > desired {
		removed, remaining := selectAllocationsToRemove(existing, int(desired))
		for _, alloc := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE id = $1`, alloc.ID); err != nil {
				return nil, err
			}
		}
		existing = remaining
	}

	if int32(len(existing)) < desired {
		needed := int(desired) - len(existing)
		occupied := make(map[string]struct{}, len(existing))
		for _, alloc := range existing {
			occupied[alloc.AgentID] = struct{}{}
		}
		placed := 0
		failureReason := "no active healthy node satisfies the placement constraints"
		for i := 0; i < needed; i++ {
			agentID, err := s.chooseReplicaAgentTx(ctx, tx, service, occupied, preferredAgentID, i == 0 && preferredAgentID != "")
			if errors.Is(err, errNoPlacementAvailable) {
				failureReason = strings.TrimSpace(strings.TrimPrefix(err.Error(), errNoPlacementAvailable.Error()+": "))
				break
			}
			if err != nil {
				return nil, err
			}
			alloc, err := s.insertAllocationTx(ctx, tx, service, agentID, now)
			if err != nil {
				return nil, err
			}
			existing = append(existing, alloc)
			occupied[agentID] = struct{}{}
			placed++
			preferredAgentID = ""
		}
		if placed < needed {
			message := pendingPlacementMessage(len(existing), int(desired), failureReason)
			if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, message, now); err != nil {
				return nil, err
			}
			service.PlacementMessage = message
			return append(existing, lost...), nil
		}
	}

	if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
		return nil, err
	}
	service.PlacementMessage = ""
	return append(existing, lost...), nil
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

func pendingCapacityMessage(placed, desired int) string {
	if placed == 0 {
		return fmt.Sprintf("0 of %d replicas placed; no healthy non-reserved agent has sufficient CPU or memory", desired)
	}
	return fmt.Sprintf("%d of %d replicas placed; no healthy non-reserved agent has sufficient CPU or memory", placed, desired)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocations(
		id, service_id, agent_id, desired_spec_revision, applied_spec_revision,
		desired_rollout_generation, applied_rollout_generation, phase, message,
		allocation_ip, healthy_ports, healthy, restart_observation_json, operator_restart_nonce,
		rollout_state, drain_started_at, drain_deadline, created_at, updated_at
	) VALUES ($1, $2, $3, $4, 0, $5, 0, 'Pending', '', '', $6, FALSE, '{}', 0, $7, NULL, NULL, $8, $8)`,
		alloc.ID, alloc.ServiceID, alloc.AgentID, alloc.DesiredSpecRevision, alloc.DesiredRolloutGeneration,
		[]byte("[]"), alloc.RolloutState, now,
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

func (s *Store) chooseReplicaAgentTx(ctx context.Context, tx *sql.Tx, service serviceRecord, occupied map[string]struct{}, preferredAgentID string, usePreferred bool) (string, error) {
	if volumeName := serviceVolumeName(service.Spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, service.EnvironmentID, volumeName); err != nil {
			return "", err
		}
	}
	if usePreferred && preferredAgentID != "" {
		if _, taken := occupied[preferredAgentID]; !taken {
			candidates, err := s.placementCandidatesQuerier(ctx, tx)
			if err != nil {
				return "", err
			}
			for _, candidate := range candidates {
				if candidate.ID == preferredAgentID && candidateEligible(candidate, service.Spec) {
					return preferredAgentID, nil
				}
			}
		}
	}
	return s.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied)
}

func (s *Store) chooseAgentForReplicaQuerier(ctx context.Context, q serviceQueryer, spec *platformv1.ServiceSpec, occupied map[string]struct{}) (string, error) {
	candidates, err := s.placementCandidatesQuerier(ctx, q)
	if err != nil {
		return "", err
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, true); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, false); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, false, false); agentID != "" {
		return agentID, nil
	}
	return "", fmt.Errorf("%w: %s", errNoPlacementAvailable, placementFailureReason(candidates, spec, s.reservedAgentIDs))
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

func (s *Store) chooseAgentForPlacementQuerier(ctx context.Context, q serviceQueryer, spec *platformv1.ServiceSpec) (string, error) {
	return s.chooseAgentForReplicaQuerier(ctx, q, spec, nil)
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
		        a.allocation_ip, a.healthy, a.updated_at, a.desired_rollout_generation,
		        a.applied_rollout_generation, a.healthy_ports, a.restart_observation_json,
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
		&rec.AllocationIP,
		&rec.Healthy,
		&rec.UpdatedAt,
		&rec.DesiredRolloutGeneration,
		&rec.AppliedRolloutGeneration,
		(*jsonInt32Slice)(&rec.HealthyPorts),
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

func encodeRestartHistory(events []allocationRestartEvent) ([]byte, error) {
	if events == nil {
		events = []allocationRestartEvent{}
	}
	return json.Marshal(events)
}

func appendAllocationRestart(current []allocationRestartEvent, event allocationRestartEvent) []allocationRestartEvent {
	out := append(append([]allocationRestartEvent(nil), current...), event)
	if len(out) > maxAllocationRestartEvents {
		out = out[len(out)-maxAllocationRestartEvents:]
	}
	return out
}

func (s *Store) recordAllocationRestartTx(ctx context.Context, tx *sql.Tx, alloc allocationRecord, reason, fromAgentID, toAgentID string, now time.Time) error {
	current := alloc
	if current.ID != "" && current.Restarts == nil {
		var history []byte
		if err := tx.QueryRowContext(ctx,
			`SELECT restart_count, last_restarted_at, restart_history, desired_rollout_generation
			   FROM allocations WHERE id = $1`,
			current.ID,
		).Scan(&current.RestartCount, &current.LastRestartedAt, &history, &current.DesiredRolloutGeneration); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if err == nil && len(history) > 0 && string(history) != "null" {
			if err := json.Unmarshal(history, &current.Restarts); err != nil {
				return err
			}
		}
	}
	if alloc.DesiredRolloutGeneration > 0 {
		current.DesiredRolloutGeneration = alloc.DesiredRolloutGeneration
	}
	history := appendAllocationRestart(current.Restarts, allocationRestartEvent{
		RestartedAt:       now,
		Reason:            reason,
		FromAgentID:       fromAgentID,
		ToAgentID:         toAgentID,
		RolloutGeneration: current.DesiredRolloutGeneration,
	})
	encoded, err := encodeRestartHistory(history)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE allocations
		    SET restart_count = restart_count + 1,
		        last_restarted_at = $1,
		        restart_history = $2,
		        updated_at = $1
		  WHERE id = $3`,
		now, encoded, alloc.ID,
	)
	return err
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

func allocationRestartObserved(prevPhase string, prevApplied int64, nextPhase string, nextApplied int64) bool {
	if prevApplied > 0 && nextApplied == 0 {
		return true
	}
	return allocationWasServing(prevPhase) && allocationIsStarting(nextPhase)
}

func allocationWasServing(phase string) bool {
	switch strings.TrimSpace(phase) {
	case "Running", "Healthy":
		return true
	default:
		return false
	}
}

func allocationIsStarting(phase string) bool {
	switch strings.TrimSpace(phase) {
	case "Pending", "Starting":
		return true
	default:
		return false
	}
}
