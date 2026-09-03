package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/restartpolicy"
)

func (s *Store) upsertAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}

		existing, err := agentByIDQuerier(ctx, tx, hello.GetAgentId(), true)
		if errors.Is(err, sql.ErrNoRows) {
			return errAgentNotEnrolled
		}
		if err != nil {
			return err
		}
		if existing.LifecycleState == agentStateRetired || existing.CredentialRevokedAt.Valid {
			return errAgentCredentialRevoked
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
		if nextState == agentStateEnrolling {
			nextState = agentStateActive
		} else if nextState == agentStateUnavailable {
			nextState = existing.StateBeforeUnavailable
			if nextState == "" || nextState == agentStateUnavailable || nextState == agentStateRetired {
				nextState = agentStateActive
			}
		}
		capabilities := canonicalCapabilities(hello.GetRuntimeCapabilities())
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
			return s.bumpAllDesiredRevisionsTx(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) heartbeatAgent(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET
		lifecycle_state = CASE WHEN lifecycle_state = 'unavailable'
			THEN CASE WHEN state_before_unavailable IN ('active', 'cordoned', 'draining') THEN state_before_unavailable ELSE 'active' END
			ELSE lifecycle_state END,
		state_before_unavailable = CASE WHEN lifecycle_state = 'unavailable' THEN '' ELSE state_before_unavailable END,
		last_seen_at = statement_timestamp(), updated_at = statement_timestamp()
		WHERE id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, agentID)
	return err
}

func (s *Store) listAgents(ctx context.Context) ([]agentRecord, error) {
	return s.listAgentsQuerier(ctx, s.db)
}

func (s *Store) agentByID(ctx context.Context, agentID string) (agentRecord, error) {
	return agentByIDQuerier(ctx, s.db, agentID, false)
}

func (s *Store) recordStatusReport(ctx context.Context, agentID string, report *agentv1.StatusReport) (bool, []string, error) {
	var ingressChanged bool
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ingressChanged = false
		changedEnvironments = make(map[string]struct{})
		rolloutServiceIDs = make(map[string]struct{})
		now := time.Now().UTC()
		for _, cond := range report.Services {
			var (
				prevAppliedSpecRevision      int64
				prevAppliedRolloutGeneration int64
				desiredSpecRevision          int64
				prevPhase                    string
				prevMessage                  string
				prevHealthy                  bool
				prevAllocationIPv4           string
				prevAllocationIPv6           string
				prevHealthyIPv4Ports         []int32
				prevHealthyIPv6Ports         []int32
				hasDomain                    bool
				environmentID                string
				serviceID                    string
				prevRestartRaw               []byte
			)
			err := tx.QueryRowContext(ctx,
				`SELECT a.applied_spec_revision,
				        a.applied_rollout_generation,
				        a.desired_spec_revision,
				        a.phase,
				        a.message,
				        a.healthy,
				        a.allocation_ipv4,
				        a.allocation_ipv6,
				        a.healthy_ipv4_ports,
				        a.healthy_ipv6_ports,
				        a.restart_observation_json,
				        EXISTS(SELECT 1 FROM domain_bindings d WHERE d.service_id = a.service_id),
				        s.environment_id,
				        a.service_id
				   FROM allocations a
				   JOIN services s ON s.id = a.service_id
				   JOIN agents ag ON ag.id = a.agent_id
				  WHERE a.id = $1 AND a.agent_id = $2`,
				cond.AllocationId, agentID,
			).Scan(
				&prevAppliedSpecRevision,
				&prevAppliedRolloutGeneration,
				&desiredSpecRevision,
				&prevPhase,
				&prevMessage,
				&prevHealthy,
				&prevAllocationIPv4,
				&prevAllocationIPv6,
				(*jsonInt32Slice)(&prevHealthyIPv4Ports),
				(*jsonInt32Slice)(&prevHealthyIPv6Ports),
				&prevRestartRaw,
				&hasDomain,
				&environmentID,
				&serviceID,
			)
			if err != nil {
				if err == sql.ErrNoRows {
					continue
				}
				return fmt.Errorf("load allocation status: %w", err)
			}
			healthyIPv4Ports, err := encodeHealthyPorts(cond.GetHealthyIpv4Ports())
			if err != nil {
				return fmt.Errorf("encode healthy IPv4 ports: %w", err)
			}
			healthyIPv6Ports, err := encodeHealthyPorts(cond.GetHealthyIpv6Ports())
			if err != nil {
				return fmt.Errorf("encode healthy IPv6 ports: %w", err)
			}
			allocationIPv4 := strings.TrimSpace(cond.GetAllocationIpv4())
			allocationIPv6 := strings.TrimSpace(cond.GetAllocationIpv6())
			if s.useReportedAllocationIP {
				if allocationIPv4 == "" {
					allocationIPv4 = prevAllocationIPv4
				}
				if allocationIPv6 == "" {
					allocationIPv6 = prevAllocationIPv6
				}
				if allocationIPv4 != "" && (net.ParseIP(allocationIPv4) == nil || net.ParseIP(allocationIPv4).To4() == nil) {
					return fmt.Errorf("reported allocation IPv4 %q is invalid", allocationIPv4)
				}
				if allocationIPv6 != "" && (net.ParseIP(allocationIPv6) == nil || net.ParseIP(allocationIPv6).To4() != nil) {
					return fmt.Errorf("reported allocation IPv6 %q is invalid", allocationIPv6)
				}
			} else {
				if allocationIPv4 != prevAllocationIPv4 || allocationIPv6 != prevAllocationIPv6 {
					return fmt.Errorf("allocation %s reported addresses %q/%q, want %q/%q", cond.GetAllocationId(), allocationIPv4, allocationIPv6, prevAllocationIPv4, prevAllocationIPv6)
				}
			}
			restartRaw, err := encodeRestartObservation(cond.GetRestart())
			if err != nil {
				return fmt.Errorf("encode restart observation: %w", err)
			}
			statusChanged := prevAppliedSpecRevision != cond.GetAppliedSpecRevision() ||
				prevAppliedRolloutGeneration != cond.GetAppliedRolloutGeneration() ||
				prevPhase != cond.GetPhase() ||
				prevMessage != cond.GetMessage() ||
				prevAllocationIPv4 != allocationIPv4 || prevAllocationIPv6 != allocationIPv6 ||
				!equalInt32Slices(prevHealthyIPv4Ports, cond.GetHealthyIpv4Ports()) ||
				!equalInt32Slices(prevHealthyIPv6Ports, cond.GetHealthyIpv6Ports()) ||
				prevHealthy != cond.GetHealthy() ||
				string(prevRestartRaw) != string(restartRaw)
			if !statusChanged {
				continue
			}
			phase := cond.Phase
			healthy := cond.Healthy
			if cond.GetRestart().GetCrashLoop() || phase == restartpolicy.PhaseCrashLoop {
				phase = restartpolicy.PhaseCrashLoop
				healthy = false
			}
			appliedSpec := cond.GetAppliedSpecRevision()
			if appliedSpec == 0 && healthy && cond.GetAppliedRolloutGeneration() >= cond.GetDesiredRolloutGeneration() {
				appliedSpec = desiredSpecRevision
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET applied_spec_revision = $1,
				        applied_rollout_generation = $2,
				        phase = $3,
				        message = $4,
				        allocation_ipv4 = $5,
				        allocation_ipv6 = $6,
				        healthy_ipv4_ports = $7,
				        healthy_ipv6_ports = $8,
				        healthy = $9,
				        restart_observation_json = $10,
				        updated_at = $11
				  WHERE id = $12 AND agent_id = $13`,
				appliedSpec, cond.AppliedRolloutGeneration, phase, cond.Message, allocationIPv4, allocationIPv6, healthyIPv4Ports, healthyIPv6Ports, healthy, restartRaw, now, cond.AllocationId, agentID,
			); err != nil {
				return fmt.Errorf("update allocation status: %w", err)
			}
			changedEnvironments[environmentID] = struct{}{}
			var rolloutState string
			err = tx.QueryRowContext(ctx,
				`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
				serviceID, cond.GetDesiredRolloutGeneration(),
			).Scan(&rolloutState)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if rolloutState == rolloutStateInProgress {
				rolloutServiceIDs[serviceID] = struct{}{}
			} else if err := s.applyAgentDeploymentObservationTx(ctx, tx, serviceID, cond.GetDesiredRolloutGeneration(), phase, cond.GetMessage(), healthy, cond.GetAppliedRolloutGeneration(), agentID); err != nil {
				return fmt.Errorf("apply deployment observation: %w", err)
			}
			if hasDomain && (prevHealthy != healthy || prevAllocationIPv4 != allocationIPv4 || prevAllocationIPv6 != allocationIPv6 || !equalInt32Slices(prevHealthyIPv4Ports, cond.GetHealthyIpv4Ports()) || !equalInt32Slices(prevHealthyIPv6Ports, cond.GetHealthyIpv6Ports()) || prevPhase != phase) {
				ingressChanged = true
			}
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	now := time.Now().UTC()
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := s.advanceRollout(ctx, serviceID, now)
		if advanceErr != nil {
			return false, nil, fmt.Errorf("advance rollout after status: %w", advanceErr)
		}
		ingressChanged = ingressChanged || advanced.IngressChanged
		if advanced.EnvironmentID != "" {
			changedEnvironments[advanced.EnvironmentID] = struct{}{}
		}
	}
	environmentIDs := make([]string, 0, len(changedEnvironments))
	for environmentID := range changedEnvironments {
		environmentIDs = append(environmentIDs, environmentID)
	}
	return ingressChanged, environmentIDs, nil
}

func (s *Store) validateAgentLogBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	type logOwner struct {
		allocationID  string
		environmentID string
		serviceID     string
	}
	seen := make(map[logOwner]struct{}, len(batch.GetEntries()))
	for _, entry := range batch.GetEntries() {
		allocationID := entry.GetAllocationId()
		if allocationID == "" {
			continue
		}
		owner := logOwner{
			allocationID:  allocationID,
			environmentID: entry.GetEnvironmentId(),
			serviceID:     entry.GetServiceId(),
		}
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1
			   FROM allocations
			  JOIN services s ON s.id = allocations.service_id
			 WHERE allocations.id = $1 AND allocations.agent_id = $2
			   AND s.environment_id = $3 AND allocations.service_id = $4`,
			allocationID, agentID, entry.GetEnvironmentId(), entry.GetServiceId(),
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("allocation %q is not assigned to agent", allocationID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) agentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agents ORDER BY created_at ASC`)
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
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) assignedNodeConfigForAgent(ctx context.Context, agentID string) (*agentv1.AssignedNodeConfig, error) {
	agent, err := s.agentByID(ctx, agentID)
	if err != nil {
		return nil, err
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		return nil, err
	}

	assigned := &agentv1.AssignedNodeConfig{
		WorkloadIpv4Subnet:     agent.WorkloadIPv4Subnet,
		WorkloadIpv4Pool:       s.mesh.WorkloadIPv4PoolCIDR,
		WorkloadIpv6Subnet:     agent.WorkloadIPv6Subnet,
		WorkloadIpv6Pool:       s.mesh.WorkloadPoolCIDR,
		WireguardInterfaceName: s.mesh.InterfaceName,
		WireguardAddresses:     []string{agent.WireGuardIPv6},
		WireguardListenPort:    int32(agent.WireGuardListenPort),
	}
	assigned.WorkloadIdentities, err = s.listWorkloadIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range agents {
		if peer.ID == agentID || peer.LifecycleState == agentStateRetired || peer.CredentialRevokedAt.Valid || peer.WireGuardPublicKey == "" || peer.WireGuardListenPort <= 0 || peer.WorkloadIPv4Subnet == "" || peer.WorkloadIPv6Subnet == "" {
			continue
		}
		endpoint, err := endpointForAgent(peer.AdvertiseAddr, peer.WireGuardListenPort)
		if err != nil {
			return nil, err
		}
		assigned.Peers = append(assigned.Peers, &agentv1.WireGuardPeer{
			AgentId:                    peer.ID,
			Name:                       peer.Name,
			PublicKey:                  peer.WireGuardPublicKey,
			Endpoint:                   endpoint,
			AllowedIps:                 []string{peer.WorkloadIPv4Subnet, peer.WorkloadIPv6Subnet},
			PersistentKeepaliveSeconds: int32(s.mesh.PersistentKeepaliveSeconds),
		})
	}
	return assigned, nil
}

func (s *Store) listWorkloadIdentities(ctx context.Context) ([]*agentv1.WorkloadIdentity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.allocation_ipv4, a.allocation_ipv6, s.environment_id, e.network_identity, a.agent_id, ag.advertise_addr
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN agents ag ON ag.id = a.agent_id
		  WHERE a.rollout_state <> $1
		  ORDER BY s.created_at ASC, a.id ASC`,
		allocationRolloutLost,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var identities []*agentv1.WorkloadIdentity
	for rows.Next() {
		var (
			workloadIPv4    string
			workloadIPv6    string
			environmentID   string
			networkIdentity int64
			hostAgentID     string
			hostIPv6        string
		)
		if err := rows.Scan(&workloadIPv4, &workloadIPv6, &environmentID, &networkIdentity, &hostAgentID, &hostIPv6); err != nil {
			return nil, err
		}
		if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", environmentID, networkIdentity)
		}
		identities = append(identities, &agentv1.WorkloadIdentity{
			WorkloadIpv4:    workloadIPv4,
			WorkloadIpv6:    workloadIPv6,
			EnvironmentId:   environmentID,
			NetworkIdentity: uint32(networkIdentity),
			HostAgentId:     hostAgentID,
			HostIpv6:        hostIPv6,
		})
	}
	return identities, rows.Err()
}

func (s *Store) allocateWorkloadSubnetTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT workload_ipv6_subnet FROM agents WHERE workload_ipv6_subnet <> ''`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		used[raw] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return nextSubnetFromPool(s.mesh.WorkloadPoolCIDR, 64, used, agentID)
}

func (s *Store) allocateWorkloadIPv4SubnetTx(ctx context.Context, tx *sql.Tx) (string, error) {
	pool := s.mesh.WorkloadIPv4PoolCIDR
	prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
	var configuredPool string
	var configuredBits int
	var nextOrdinal int64
	err := tx.QueryRowContext(ctx, `SELECT pool_cidr, prefix_bits, next_ordinal
		FROM workload_ipv4_prefix_allocator WHERE id = TRUE FOR UPDATE`).Scan(&configuredPool, &configuredBits, &nextOrdinal)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workload_ipv4_prefix_allocator(id, pool_cidr, prefix_bits, next_ordinal)
			VALUES (TRUE, $1, $2, 0)`, pool, prefixBits); err != nil {
			return "", err
		}
		configuredPool, configuredBits, nextOrdinal = pool, prefixBits, 0
	} else if err != nil {
		return "", err
	}
	if configuredPool != pool || configuredBits != prefixBits {
		return "", fmt.Errorf("IPv4 workload pool configuration changed from %s /%d to %s /%d", configuredPool, configuredBits, pool, prefixBits)
	}

	poolPrefix, err := netip.ParsePrefix(pool)
	if err != nil || !poolPrefix.Addr().Is4() {
		return "", fmt.Errorf("invalid IPv4 workload pool %q", pool)
	}
	poolPrefix = poolPrefix.Masked()
	rows, err := tx.QueryContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE workload_ipv4_subnet <> '' ORDER BY workload_ipv4_subnet`)
	if err != nil {
		return "", err
	}
	var used []netip.Prefix
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return "", err
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() != prefixBits || !poolPrefix.Contains(prefix.Masked().Addr()) {
			rows.Close()
			return "", fmt.Errorf("allocated IPv4 node prefix %q is outside configured pool %q", raw, pool)
		}
		prefix = prefix.Masked()
		for _, other := range used {
			if ipv4PrefixesOverlap(prefix, other) {
				rows.Close()
				return "", fmt.Errorf("allocated IPv4 node prefixes %s and %s overlap", prefix, other)
			}
		}
		used = append(used, prefix)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	candidateRaw, err := ipv4SubnetAt(pool, prefixBits, uint64(nextOrdinal))
	if err != nil {
		return "", err
	}
	candidate, _ := netip.ParsePrefix(candidateRaw)
	for _, other := range used {
		if ipv4PrefixesOverlap(candidate, other) {
			return "", fmt.Errorf("next IPv4 node prefix %s overlaps allocated prefix %s", candidate, other)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workload_ipv4_prefix_allocator SET next_ordinal = $1 WHERE id = TRUE`, nextOrdinal+1); err != nil {
		return "", err
	}
	return candidateRaw, nil
}

func (s *Store) validateWorkloadIPv4Pool(ctx context.Context) error {
	return s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		pool := s.mesh.WorkloadIPv4PoolCIDR
		prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
		if strings.TrimSpace(pool) == "" || prefixBits == 0 {
			return errors.New("IPv4 workload pool and per-node prefix size are required")
		}
		if _, err := ipv4SubnetAt(pool, prefixBits, 0); err != nil {
			return err
		}
		var configuredPool string
		var configuredBits int
		var nextOrdinal int64
		err := tx.QueryRowContext(ctx, `SELECT pool_cidr, prefix_bits, next_ordinal
			FROM workload_ipv4_prefix_allocator WHERE id = TRUE FOR UPDATE`).Scan(&configuredPool, &configuredBits, &nextOrdinal)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO workload_ipv4_prefix_allocator(id, pool_cidr, prefix_bits, next_ordinal)
				VALUES (TRUE, $1, $2, 0)`, pool, prefixBits)
			return err
		}
		if err != nil {
			return err
		}
		if configuredPool != pool || configuredBits != prefixBits {
			return fmt.Errorf("IPv4 workload pool configuration changed from %s /%d to %s /%d", configuredPool, configuredBits, pool, prefixBits)
		}
		poolPrefix, err := netip.ParsePrefix(pool)
		if err != nil || !poolPrefix.Addr().Is4() {
			return fmt.Errorf("invalid IPv4 workload pool %q", pool)
		}
		poolPrefix = poolPrefix.Masked()
		rows, err := tx.QueryContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE workload_ipv4_subnet <> '' ORDER BY workload_ipv4_subnet FOR UPDATE`)
		if err != nil {
			return err
		}
		var allocated []netip.Prefix
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() != prefixBits || !poolPrefix.Contains(prefix.Addr()) {
				rows.Close()
				return fmt.Errorf("allocated IPv4 node prefix %q is outside configured pool %q", raw, pool)
			}
			for _, other := range allocated {
				if ipv4PrefixesOverlap(prefix, other) {
					rows.Close()
					return fmt.Errorf("allocated IPv4 node prefixes %s and %s overlap", prefix, other)
				}
			}
			allocated = append(allocated, prefix)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if nextOrdinal < 0 {
			return fmt.Errorf("IPv4 workload allocator has invalid next ordinal %d", nextOrdinal)
		}
		if _, err := ipv4SubnetAt(pool, prefixBits, uint64(nextOrdinal)); err != nil && len(allocated) == 0 && nextOrdinal == 0 {
			return err
		}
		return nil
	})
}

func (s *Store) allocateWorkloadIPv4AddressTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	var subnet string
	if err := tx.QueryRowContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&subnet); err != nil {
		return "", err
	}
	if strings.TrimSpace(subnet) == "" {
		return "", fmt.Errorf("agent %s has no IPv4 workload prefix", agentID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT allocation_ipv4 FROM allocations
		WHERE agent_id = $1 AND rollout_state <> $2 AND allocation_ipv4 <> ''`, agentID, allocationRolloutLost)
	if err != nil {
		return "", err
	}
	used := make(map[string]struct{})
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			rows.Close()
			return "", err
		}
		used[addr] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	return nextIPv4AddressFromSubnet(subnet, used)
}

func (s *Store) backfillWorkloadIPv4AddressesTx(ctx context.Context, tx *sql.Tx, agentID, subnet string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, allocation_ipv4 FROM allocations
		WHERE agent_id = $1 AND rollout_state <> $2 ORDER BY created_at ASC, id ASC FOR UPDATE`, agentID, allocationRolloutLost)
	if err != nil {
		return false, err
	}
	used := make(map[string]struct{})
	var missing []string
	for rows.Next() {
		var allocationID, address string
		if err := rows.Scan(&allocationID, &address); err != nil {
			rows.Close()
			return false, err
		}
		address = strings.TrimSpace(address)
		if address == "" {
			missing = append(missing, allocationID)
			continue
		}
		used[address] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, allocationID := range missing {
		address, err := nextIPv4AddressFromSubnet(subnet, used)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocations SET allocation_ipv4 = $1, updated_at = statement_timestamp() WHERE id = $2`, address, allocationID); err != nil {
			return false, err
		}
		used[address] = struct{}{}
	}
	return len(missing) > 0, nil
}

func (s *Store) allocateWireGuardIPv6Tx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT wireguard_ipv6 FROM agents WHERE wireguard_ipv6 <> ''`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		used[raw] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return nextAddressFromPool(s.mesh.NetworkCIDR, used, 0x10, agentID)
}

func (s *Store) schedulerSnapshot(ctx context.Context) ([]agentRecord, []serviceRecord, error) {
	return s.schedulerSnapshotTx(ctx, s.db)
}

func (s *Store) schedulerSnapshotTx(ctx context.Context, q serviceQueryer) ([]agentRecord, []serviceRecord, error) {
	agents, err := s.listAgentsQuerier(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.QueryContext(ctx,
		serviceSelectSQL+`
		  ORDER BY s.created_at ASC`,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var services []serviceRecord
	for rows.Next() {
		rec, err := scanServiceRow(rows)
		if err != nil {
			return nil, nil, err
		}
		services = append(services, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	for i := range services {
		spec, err := s.loadServiceDetailsQuerier(ctx, q, services[i].ID, services[i].SpecRevision)
		if err != nil && err != sql.ErrNoRows {
			return nil, nil, err
		}
		services[i].Spec = spec
	}
	return agents, services, nil
}

func (s *Store) listAgentsQuerier(ctx context.Context, q serviceQueryer) ([]agentRecord, error) {
	rows, err := q.QueryContext(ctx,
		agentSelectSQL+`
		  ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []agentRecord
	for rows.Next() {
		rec, err := scanAgentRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
