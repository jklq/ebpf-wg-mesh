package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// reconcileDrainingAgent replaces stateless allocations on a draining node by
// starting targeted rolling replacements. Volume-backed allocations remain
// fenced until the Stage 7 handoff contract exists. Existing allocation rows
// are never rewritten onto another agent.
func (d *Delivery) reconcileDrainingAgent(ctx context.Context, agentID string) ([]string, error) {
	s := d.store
	var notify []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		notify = nil
		agent, err := agentByIDQuerier(ctx, tx, agentID, true)
		if err != nil {
			return err
		}
		if agent.LifecycleState != AgentStateDraining {
			return nil
		}
		allocations, err := listAllocationsForFailover(ctx, tx, agentID)
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
			if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
				blocked[fmt.Sprintf("stateful allocation %s is fenced to volume %q until Stage 7 handoff is available", allocation.ID, volumeName)] = struct{}{}
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
			if !ok || current.ResolvedSpec == nil || strings.TrimSpace(current.ImageDigest) == "" {
				blocked["service has no reusable image snapshot for a rolling replacement"] = struct{}{}
				continue
			}
			detail := fmt.Sprintf("Maintenance drain replacing allocation %s from %s", allocation.ID, agentID)
			if _, err := d.copyDeploymentRolloutTargetTx(ctx, tx, service, current, "", reasonAgentDrain, detail, allocation.ID); err != nil {
				switch {
				case errors.Is(err, ErrRolloutInProgress):
					blocked["waiting for an in-progress rollout to finish"] = struct{}{}
				case errors.Is(err, ErrVolumeRollingUnsupported):
					blocked[fmt.Sprintf("stateful allocation %s cannot overlap generations until Stage 7 handoff is available", allocation.ID)] = struct{}{}
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
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET maintenance_message = $1, updated_at = $2 WHERE id = $3 AND lifecycle_state = 'draining'`, message, now, agentID); err != nil {
			return err
		}
		if !started {
			return nil
		}
		if err := dbtx.BumpAllDesiredRevisions(ctx, tx); err != nil {
			return err
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

// reconcileFleetCapacity retries pending replica placement and interrupted
// drains whenever observed fleet capacity changes (for example a node return).
func (d *Delivery) ReconcileFleetCapacity(ctx context.Context) error {
	s := d.store
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE placement_message <> '' ORDER BY id FOR UPDATE`)
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
		if changed {
			return dbtx.BumpAllDesiredRevisions(ctx, tx)
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

func (d *Delivery) CreateFleetAgent(ctx context.Context, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return AgentRecord{}, "", err
	}
	return d.createFleetAgent(ctx, identity.UserID, req)
}

func (d *Delivery) createFleetAgent(ctx context.Context, userID string, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return AgentRecord{}, "", err
	}
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, "", err
	}
	token, err := newAgentBootstrapToken()
	if err != nil {
		return AgentRecord{}, "", err
	}
	var rec AgentRecord
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `INSERT INTO agents(
			id, name, lifecycle_state, region, zone, failure_domain,
			reserved_cpu_millis, reserved_memory_mebibytes, last_seen_at, created_at, updated_at
		) VALUES ($1, $2, 'enrolling', $3, $4, $5, $6, $7, $8, $9, $9)`,
			strings.TrimSpace(req.GetAgentId()), strings.TrimSpace(req.GetName()),
			strings.TrimSpace(req.GetRegion()), strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()),
			req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes(), time.Unix(0, 0).UTC(), now)
		if err != nil {
			return err
		}
		if err := insertAgentBootstrapTokenTx(ctx, tx, req.GetAgentId(), token, "operator", now); err != nil {
			return err
		}
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, token, err
}

func (d *Delivery) UpdateFleetAgent(ctx context.Context, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return AgentRecord{}, err
	}
	return d.updateFleetAgent(ctx, identity.UserID, req)
}

func (d *Delivery) updateFleetAgent(ctx context.Context, userID string, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return AgentRecord{}, err
	}
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, err
	}
	var rec AgentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := agentByIDQuerier(ctx, tx, req.GetAgentId(), true)
		if err != nil {
			return err
		}
		if current.LifecycleState == AgentStateRetired {
			return fmt.Errorf("%w: retired agents are immutable", ErrInvalidAgentTransition)
		}
		if req.GetReservedCpuMillis() > current.CPUMillisCapacity && current.CPUMillisCapacity > 0 {
			return errors.New("reserved CPU exceeds observed node capacity")
		}
		if req.GetReservedMemoryMebibytes() > current.MemoryMebibytesCapcity && current.MemoryMebibytesCapcity > 0 {
			return errors.New("reserved memory exceeds observed node capacity")
		}
		_, err = tx.ExecContext(ctx, `UPDATE agents SET name = $1, region = $2, zone = $3,
			failure_domain = $4, reserved_cpu_millis = $5, reserved_memory_mebibytes = $6,
			updated_at = $7 WHERE id = $8`, strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetRegion()),
			strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()), req.GetReservedCpuMillis(),
			req.GetReservedMemoryMebibytes(), time.Now().UTC(), req.GetAgentId())
		if err != nil {
			return err
		}
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, err
}

func (d *Delivery) RegisterAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	s := d.store
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}

		existing, err := agentByIDQuerier(ctx, tx, hello.GetAgentId(), true)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAgentNotEnrolled
		}
		if err != nil {
			return err
		}
		if existing.LifecycleState == AgentStateRetired || existing.CredentialRevokedAt.Valid {
			return ErrAgentCredentialRevoked
		}

		workloadIPv4Subnet := existing.WorkloadIPv4Subnet
		if workloadIPv4Subnet == "" {
			workloadIPv4Subnet, err = s.allocateWorkloadIPv4SubnetTx(ctx, tx)
			if err != nil {
				return err
			}
		}
		addressesBackfilled, err := s.backfillWorkloadIPv4AddressesTx(ctx, tx, existing.ID, workloadIPv4Subnet)
		if err != nil {
			return err
		}
		workloadSubnet := existing.WorkloadIPv6Subnet
		if workloadSubnet == "" {
			workloadSubnet, err = s.allocateWorkloadSubnetTx(ctx, tx, hello.AgentId)
			if err != nil {
				return err
			}
		}
		wireGuardIPv6 := existing.WireGuardIPv6
		if wireGuardIPv6 == "" {
			wireGuardIPv6, err = s.allocateWireGuardIPv6Tx(ctx, tx, hello.AgentId)
			if err != nil {
				return err
			}
		}

		nextState := existing.LifecycleState
		if nextState == AgentStateEnrolling {
			nextState = AgentStateActive
		} else if nextState == AgentStateUnavailable {
			nextState = existing.StateBeforeUnavailable
			if nextState == "" || nextState == AgentStateUnavailable || nextState == AgentStateRetired {
				nextState = AgentStateActive
			}
		}
		capabilities := CanonicalCapabilities(hello.GetRuntimeCapabilities())
		changed = existing.AdvertiseAddr != hello.AdvertiseAddr ||
			existing.WireGuardPublicKey != hello.GetWireguardPublicKey() ||
			existing.WireGuardListenPort != int(hello.GetWireguardListenPort()) ||
			existing.CPUMillisCapacity != hello.CpuMillisCapacity ||
			existing.MemoryMebibytesCapcity != hello.MemoryMebibytesCapacity ||
			!slices.Equal(existing.RuntimeCapabilities, capabilities) ||
			existing.SoftwareVersion != strings.TrimSpace(hello.GetSoftwareVersion()) ||
			existing.LifecycleState != nextState ||
			addressesBackfilled ||
			existing.WorkloadIPv4Subnet != workloadIPv4Subnet ||
			existing.WorkloadIPv6Subnet != workloadSubnet ||
			existing.WireGuardIPv6 != wireGuardIPv6

		capabilitiesJSON, err := json.Marshal(capabilities)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE agents SET lifecycle_state = $1, state_before_unavailable = '',
				advertise_addr = $2, workload_ipv4_subnet = $3, workload_ipv6_subnet = $4, wireguard_public_key = $5,
				wireguard_listen_port = $6, wireguard_ipv6 = $7, cpu_millis_capacity = $8,
				memory_mebibytes_capacity = $9, runtime_capabilities = $10,
				software_version = $11, last_seen_at = $12, updated_at = $12
			 WHERE id = $13`,
			nextState,
			hello.AdvertiseAddr,
			workloadIPv4Subnet,
			workloadSubnet,
			hello.GetWireguardPublicKey(),
			hello.GetWireguardListenPort(),
			wireGuardIPv6,
			hello.CpuMillisCapacity,
			hello.MemoryMebibytesCapacity,
			capabilitiesJSON,
			strings.TrimSpace(hello.GetSoftwareVersion()),
			now,
			hello.AgentId,
		)
		if err != nil {
			return err
		}
		if changed {
			return dbtx.BumpAllDesiredRevisions(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
