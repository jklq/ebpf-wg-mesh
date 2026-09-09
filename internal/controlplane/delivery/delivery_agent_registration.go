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
