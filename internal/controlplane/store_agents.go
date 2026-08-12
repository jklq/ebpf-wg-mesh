package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func (s *Store) upsertAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()

		var existing agentRecord
		err := tx.QueryRowContext(ctx,
			`SELECT id, name, advertise_addr, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
			        wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity, last_seen_at
			   FROM agents
			  WHERE id = $1`,
			hello.AgentId,
		).Scan(
			&existing.ID,
			&existing.Name,
			&existing.AdvertiseAddr,
			&existing.WorkloadIPv6Subnet,
			&existing.WireGuardPublicKey,
			&existing.WireGuardListenPort,
			&existing.WireGuardIPv6,
			&existing.CPUMillisCapacity,
			&existing.MemoryMebibytesCapcity,
			&existing.LastSeenAt,
		)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		isNew := err == sql.ErrNoRows

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

		changed = isNew ||
			existing.Name != hello.Name ||
			existing.AdvertiseAddr != hello.AdvertiseAddr ||
			existing.WireGuardPublicKey != hello.GetWireguardPublicKey() ||
			existing.WireGuardListenPort != int(hello.GetWireguardListenPort()) ||
			existing.CPUMillisCapacity != hello.CpuMillisCapacity ||
			existing.MemoryMebibytesCapcity != hello.MemoryMebibytesCapacity ||
			existing.WorkloadIPv6Subnet != workloadSubnet ||
			existing.WireGuardIPv6 != wireGuardIPv6

		_, err = tx.ExecContext(ctx,
			`INSERT INTO agents(
				id, name, advertise_addr, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
				wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity, last_seen_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, $10, $11, $12
			)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				advertise_addr = excluded.advertise_addr,
				workload_ipv6_subnet = excluded.workload_ipv6_subnet,
				wireguard_public_key = excluded.wireguard_public_key,
				wireguard_listen_port = excluded.wireguard_listen_port,
				wireguard_ipv6 = excluded.wireguard_ipv6,
				cpu_millis_capacity = excluded.cpu_millis_capacity,
				memory_mebibytes_capacity = excluded.memory_mebibytes_capacity,
				last_seen_at = excluded.last_seen_at,
				updated_at = excluded.updated_at`,
			hello.AgentId,
			hello.Name,
			hello.AdvertiseAddr,
			workloadSubnet,
			hello.GetWireguardPublicKey(),
			hello.GetWireguardListenPort(),
			wireGuardIPv6,
			hello.CpuMillisCapacity,
			hello.MemoryMebibytesCapacity,
			now,
			now,
			now,
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
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1, updated_at = $2 WHERE id = $3`, now, now, agentID)
	return err
}

func (s *Store) listAgents(ctx context.Context) ([]agentRecord, error) {
	return s.listAgentsQuerier(ctx, s.db)
}

func (s *Store) agentByID(ctx context.Context, agentID string) (agentRecord, error) {
	var rec agentRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, advertise_addr, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
		        wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity, last_seen_at
		   FROM agents
		  WHERE id = $1`,
		agentID,
	).Scan(
		&rec.ID,
		&rec.Name,
		&rec.AdvertiseAddr,
		&rec.WorkloadIPv6Subnet,
		&rec.WireGuardPublicKey,
		&rec.WireGuardListenPort,
		&rec.WireGuardIPv6,
		&rec.CPUMillisCapacity,
		&rec.MemoryMebibytesCapcity,
		&rec.LastSeenAt,
	)
	if err != nil {
		return agentRecord{}, err
	}
	return rec, nil
}

func (s *Store) recordStatusReport(ctx context.Context, agentID string, report *agentv1.StatusReport) (bool, []string, error) {
	var ingressChanged bool
	changedEnvironments := make(map[string]struct{})
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ingressChanged = false
		changedEnvironments = make(map[string]struct{})
		now := time.Now().UTC()
		for _, cond := range report.Services {
			var (
				prevAppliedSpecRevision      int64
				prevAppliedRolloutGeneration int64
				prevPhase                    string
				prevMessage                  string
				prevHealthy                  bool
				prevAllocationIP             string
				prevHealthyPorts             []int32
				hasDomain                    bool
				workloadSubnet               string
				environmentID                string
				serviceID                    string
			)
			err := tx.QueryRowContext(ctx,
				`SELECT a.applied_spec_revision,
				        a.applied_rollout_generation,
				        a.phase,
				        a.message,
				        a.healthy,
				        a.allocation_ip,
				        a.healthy_ports,
				        EXISTS(SELECT 1 FROM domain_bindings d WHERE d.service_id = a.service_id),
				        ag.workload_ipv6_subnet,
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
				&prevPhase,
				&prevMessage,
				&prevHealthy,
				&prevAllocationIP,
				(*jsonInt32Slice)(&prevHealthyPorts),
				&hasDomain,
				&workloadSubnet,
				&environmentID,
				&serviceID,
			)
			if err != nil {
				if err == sql.ErrNoRows {
					continue
				}
				return fmt.Errorf("load allocation status: %w", err)
			}
			healthyPorts, err := encodeHealthyPorts(cond.GetHealthyPorts())
			if err != nil {
				return fmt.Errorf("encode healthy ports: %w", err)
			}
			allocationIP := strings.TrimSpace(cond.GetAllocationIp())
			if s.useReportedAllocationIP {
				if net.ParseIP(allocationIP) == nil {
					return fmt.Errorf("reported allocation ip %q is invalid", allocationIP)
				}
			} else {
				allocationIP, err = privateIPv6(workloadSubnet, environmentID, serviceID)
				if err != nil {
					return fmt.Errorf("derive allocation ip: %w", err)
				}
			}
			statusChanged := prevAppliedSpecRevision != cond.GetAppliedSpecRevision() ||
				prevAppliedRolloutGeneration != cond.GetAppliedRolloutGeneration() ||
				prevPhase != cond.GetPhase() ||
				prevMessage != cond.GetMessage() ||
				prevAllocationIP != allocationIP ||
				!equalInt32Slices(prevHealthyPorts, cond.GetHealthyPorts()) ||
				prevHealthy != cond.GetHealthy()
			if !statusChanged {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET applied_spec_revision = $1,
				        applied_rollout_generation = $2,
				        phase = $3,
				        message = $4,
				        allocation_ip = $5,
				        healthy_ports = $6,
				        healthy = $7,
				        updated_at = $8
				  WHERE id = $9 AND agent_id = $10`,
				cond.AppliedSpecRevision, cond.AppliedRolloutGeneration, cond.Phase, cond.Message, allocationIP, healthyPorts, cond.Healthy, now, cond.AllocationId, agentID,
			); err != nil {
				return fmt.Errorf("update allocation status: %w", err)
			}
			changedEnvironments[environmentID] = struct{}{}
			if hasDomain && (prevHealthy != cond.Healthy || prevAllocationIP != allocationIP || !equalInt32Slices(prevHealthyPorts, cond.GetHealthyPorts())) {
				ingressChanged = true
			}
		}
		return nil
	})
	if err != nil {
		return false, nil, err
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
		if peer.ID == agentID || peer.WireGuardPublicKey == "" || peer.WireGuardListenPort <= 0 || peer.WorkloadIPv6Subnet == "" {
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
			AllowedIps:                 []string{peer.WorkloadIPv6Subnet},
			PersistentKeepaliveSeconds: int32(s.mesh.PersistentKeepaliveSeconds),
		})
	}
	return assigned, nil
}

func (s *Store) listWorkloadIdentities(ctx context.Context) ([]*agentv1.WorkloadIdentity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.environment_id, e.network_identity, a.agent_id, ag.advertise_addr, ag.workload_ipv6_subnet
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN agents ag ON ag.id = a.agent_id
		  ORDER BY s.created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var identities []*agentv1.WorkloadIdentity
	for rows.Next() {
		var (
			serviceID       string
			environmentID   string
			networkIdentity int64
			hostAgentID     string
			hostIPv6        string
			workloadSubnet  string
		)
		if err := rows.Scan(&serviceID, &environmentID, &networkIdentity, &hostAgentID, &hostIPv6, &workloadSubnet); err != nil {
			return nil, err
		}
		if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", environmentID, networkIdentity)
		}
		workloadIPv6, err := privateIPv6(workloadSubnet, environmentID, serviceID)
		if err != nil {
			return nil, err
		}
		identities = append(identities, &agentv1.WorkloadIdentity{
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
		`SELECT s.id, s.environment_id, e.project_id, s.name, s.current_spec_revision,
		        s.current_rollout_generation, COALESCE(a.agent_id, ''), s.current_resolved_image,
		        s.last_successful_commit_sha, s.latest_build_id, s.created_at, s.updated_at
		   FROM services s JOIN environments e ON e.id = s.environment_id
		   LEFT JOIN allocations a ON a.service_id = s.id
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
		`SELECT id, name, advertise_addr, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
		        wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity, last_seen_at
		   FROM agents
		  ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []agentRecord
	for rows.Next() {
		var rec agentRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.Name,
			&rec.AdvertiseAddr,
			&rec.WorkloadIPv6Subnet,
			&rec.WireGuardPublicKey,
			&rec.WireGuardListenPort,
			&rec.WireGuardIPv6,
			&rec.CPUMillisCapacity,
			&rec.MemoryMebibytesCapcity,
			&rec.LastSeenAt,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
