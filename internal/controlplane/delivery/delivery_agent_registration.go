package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
)

func CanonicalAgentWireGuardEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	endpoint, err := netip.ParseAddrPort(raw)
	if err != nil {
		return "", fmt.Errorf("wireguard_endpoint must be an IP:port endpoint: %q", raw)
	}
	if endpoint.Port() == 0 {
		return "", fmt.Errorf("wireguard_endpoint must include a non-zero port: %q", raw)
	}
	addr := endpoint.Addr().Unmap()
	if !isUsableUnderlayAddress(addr) {
		return "", fmt.Errorf("wireguard_endpoint must be a routable unicast address: %q", raw)
	}
	return netip.AddrPortFrom(addr, endpoint.Port()).String(), nil
}

func CanonicalAgentAdvertiseAddr(raw string) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("advertise_addr must be an IPv6 address: %q", raw)
	}
	addr = addr.Unmap()
	if !addr.Is6() {
		return "", fmt.Errorf("advertise_addr must be an IPv6 address: %q", raw)
	}
	if !isUsableUnderlayAddress(addr) {
		return "", fmt.Errorf("advertise_addr must be a routable unicast address: %q", raw)
	}
	return addr.String(), nil
}

// isUsableUnderlayAddress rejects unspecified, loopback, link-local and
// multicast addresses so an agent cannot redirect peer handshakes at martian
// or local-only destinations.
func isUsableUnderlayAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return false
	}
	return true
}

func (d *Delivery) RegisterAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	if hello.GetWireguardListenPort() < 1 || hello.GetWireguardListenPort() > math.MaxUint16 {
		return false, fmt.Errorf("wireguard listen port must be between 1 and 65535")
	}
	// Persisted agent metadata remains active across a control-plane restart, but
	// the missing live session still makes the agent unavailable to placement.
	becameReachable := true
	if agent, ok := d.live.Agent(hello.GetAgentId()); ok {
		becameReachable = agent.LifecycleState == AgentStateUnavailable
	}
	s := d.store
	var changed bool
	var wireGuardEndpoint string
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		wireGuardEndpoint, err = CanonicalAgentWireGuardEndpoint(hello.GetWireguardEndpoint())
		if err != nil {
			return err
		}
		advertiseAddr, err := CanonicalAgentAdvertiseAddr(hello.GetAdvertiseAddr())
		if err != nil {
			return err
		}
		hello.AdvertiseAddr = advertiseAddr

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
		journal.RecordAgent(ctx, hello.GetAgentId())
		if storeID == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET local_store_id = $2 WHERE id = $1`, hello.GetAgentId(), hello.GetLocalStoreId()); err != nil {
				return err
			}
			journal.RecordAgent(ctx, hello.GetAgentId())
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
			existing.WireGuardEndpoint != wireGuardEndpoint ||
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
				wireguard_listen_port = $5, wireguard_endpoint = $6, wireguard_ipv6 = $7, cpu_millis_capacity = $8,
				memory_mebibytes_capacity = $9, runtime_capabilities = $10,
				software_version = $11, updated_at = $12
			 WHERE id = $13`,
			hello.AdvertiseAddr,
			workloadIPv4Subnet,
			workloadSubnet,
			hello.GetWireguardPublicKey(),
			hello.GetWireguardListenPort(),
			wireGuardEndpoint,
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
		journal.RecordAgent(ctx, hello.GetAgentId())
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
		journal.RecordAdministration(ctx, hello.GetAgentId())
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
	return changed || becameReachable, nil
}

func assignedAllocationIDs(d *Delivery, ctx context.Context, agentID string) []string {
	_ = ctx
	if d == nil || d.live == nil {
		return nil
	}
	return d.live.AssignedIDs(agentID)
}

func (d *Delivery) ObserveAgentHeartbeat(ctx context.Context, agentID, sessionID string, recoveryMode bool) error {
	_ = ctx
	return d.live.Heartbeat(agentID, sessionID, !recoveryMode)
}

func (d *Delivery) EndAgentSession(ctx context.Context, agentID, sessionID string) error {
	_ = ctx
	return d.live.EndSession(agentID, sessionID)
}
