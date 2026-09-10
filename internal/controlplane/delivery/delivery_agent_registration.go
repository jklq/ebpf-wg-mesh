package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
)

func (d *Delivery) RegisterAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	s := d.store
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}

		var lockedAgentID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_registrations WHERE id = $1 FOR UPDATE`, hello.GetAgentId()).Scan(&lockedAgentID); errors.Is(err, sql.ErrNoRows) {
			return ErrAgentNotEnrolled
		} else if err != nil {
			return err
		}
		existing, err := agentByIDQuerier(ctx, tx, lockedAgentID, false)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAgentNotEnrolled
		}
		if err != nil {
			return err
		}
		if existing.LifecycleState == AgentStateRetired || existing.CredentialRevokedAt.Valid {
			return ErrAgentCredentialRevoked
		}

		// Bind enrollment to one durable store. An empty replacement store is
		// not evidence that the old process or its workloads have stopped.
		if hello.GetLocalStoreId() == "" {
			return fmt.Errorf("local_store_id is required")
		}
		var storeID string
		var previousIncarnation uint64
		if err := tx.QueryRowContext(ctx, `SELECT local_store_id, session_incarnation FROM agent_registrations WHERE id = $1`, hello.GetAgentId()).Scan(&storeID, &previousIncarnation); err != nil {
			return err
		}
		if storeID != "" && storeID != hello.GetLocalStoreId() {
			return fmt.Errorf("%w: local store differs from enrolled store", reconciliation.ErrIdentityRecovery)
		}
		if hello.GetSessionIncarnation() <= previousIncarnation || hello.GetSessionIncarnation() > math.MaxInt64 {
			return ErrStaleAgentSession
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET session_incarnation = $2 WHERE id = $1`, hello.GetAgentId(), int64(hello.GetSessionIncarnation())); err != nil {
			return err
		}
		if storeID == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET local_store_id = $2 WHERE id = $1`, hello.GetAgentId(), hello.GetLocalStoreId()); err != nil {
				return err
			}
		}
		workloadIPv4Subnet := existing.WorkloadIPv4Subnet
		if workloadIPv4Subnet == "" {
			workloadIPv4Subnet, err = s.allocateWorkloadIPv4SubnetTx(ctx, tx)
			if err != nil {
				return err
			}
		}
		addressesBackfilled, err := d.backfillWorkloadIPv4AddressesTx(ctx, tx, existing.ID, workloadIPv4Subnet, now)
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

		capabilities := CanonicalCapabilities(hello.GetRuntimeCapabilities())
		changed = existing.AdvertiseAddr != hello.AdvertiseAddr ||
			existing.WireGuardPublicKey != hello.GetWireguardPublicKey() ||
			existing.WireGuardListenPort != int(hello.GetWireguardListenPort()) ||
			existing.CPUMillisCapacity != hello.CpuMillisCapacity ||
			existing.MemoryMebibytesCapcity != hello.MemoryMebibytesCapacity ||
			!slices.Equal(existing.RuntimeCapabilities, capabilities) ||
			existing.SoftwareVersion != strings.TrimSpace(hello.GetSoftwareVersion()) ||
			existing.LifecycleState == AgentStateEnrolling || existing.LifecycleState == AgentStateUnavailable ||
			addressesBackfilled ||
			existing.WorkloadIPv4Subnet != workloadIPv4Subnet ||
			existing.WorkloadIPv6Subnet != workloadSubnet ||
			existing.WireGuardIPv6 != wireGuardIPv6

		capabilitiesJSON, err := json.Marshal(capabilities)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE agent_registrations SET
				advertise_addr = $1, workload_ipv4_subnet = $2, workload_ipv6_subnet = $3, wireguard_public_key = $4,
				wireguard_listen_port = $5, wireguard_ipv6 = $6, cpu_millis_capacity = $7,
				memory_mebibytes_capacity = $8, runtime_capabilities = $9,
				software_version = $10, updated_at = $11
			 WHERE id = $12`,
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
			now, hello.AgentId,
		)
		if err != nil {
			return err
		}
		administration, err := tx.ExecContext(ctx, `UPDATE agent_administration
			SET lifecycle_state = CASE WHEN lifecycle_state = 'enrolling' THEN 'active' ELSE lifecycle_state END,
			    updated_at = CASE WHEN lifecycle_state = 'enrolling' THEN $2 ELSE updated_at END
			WHERE agent_id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, hello.GetAgentId(), now)
		if err != nil {
			return err
		}
		rows, err := administration.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrAgentCredentialRevoked
		}
		if changed {
			return dbtx.BumpAllDesiredRevisions(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	assigned := assignedAllocationIDs(d, ctx, hello.GetAgentId())
	inventory := make([]string, 0, len(hello.GetAllocations()))
	for _, cond := range hello.GetAllocations() {
		inventory = append(inventory, cond.GetAllocationId())
	}
	if err := d.live.BeginSession(hello.GetAgentId(), hello.GetSessionId(), inventory, assigned, !hello.GetRecoveryMode()); err != nil {
		return false, err
	}
	return changed, nil
}

func assignedAllocationIDs(d *Delivery, ctx context.Context, agentID string) []string {
	var ids []string
	_ = d.store.readState(ctx, func(_ *sql.Tx, state journal.DurableState) error {
		for _, assignment := range state.Assignments {
			if assignment.AgentID == agentID && assignment.RolloutState != AllocationRolloutLost {
				ids = append(ids, assignment.ID)
			}
		}
		return nil
	})
	return ids
}

// ObserveAgentHeartbeat refreshes presence only for the currently registered
// session incarnation.
func (d *Delivery) ObserveAgentHeartbeat(ctx context.Context, agentID, sessionID string, recoveryMode bool) error {
	_ = ctx
	return d.live.Heartbeat(agentID, sessionID, !recoveryMode)
}

// EndAgentSession makes disconnects visible immediately. The session predicate
// prevents an old stream's cleanup from fencing a replacement session.
func (d *Delivery) EndAgentSession(ctx context.Context, agentID, sessionID string) error {
	_ = ctx
	return d.live.EndSession(agentID, sessionID)
}
