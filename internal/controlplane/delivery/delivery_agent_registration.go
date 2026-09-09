package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
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
		if err := s.beginAgentSessionTx(ctx, tx, hello.GetAgentId(), hello.GetSessionId(), now); err != nil {
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

// ObserveAgentHeartbeat refreshes presence only for the currently registered
// session incarnation.
func (d *Delivery) ObserveAgentHeartbeat(ctx context.Context, agentID, sessionID string) error {
	return d.store.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return d.store.recordAgentContactTx(ctx, tx, agentID, sessionID, now)
	})
}

// EndAgentSession makes disconnects visible immediately. The session predicate
// prevents an old stream's cleanup from fencing a replacement session.
func (d *Delivery) EndAgentSession(ctx context.Context, agentID, sessionID string) error {
	return d.store.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return d.store.endAgentSessionTx(ctx, tx, agentID, sessionID, now)
	})
}
