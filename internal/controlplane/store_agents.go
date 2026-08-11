package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func (s *Store) recordStatusReport(ctx context.Context, agentID string, report *agentv1.StatusReport) (bool, error) {
	var ingressChanged bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		for _, cond := range report.Services {
			var (
				prevHealthy      bool
				prevAllocationIP string
				prevHealthyPorts []int32
				hasDomain        bool
				workloadSubnet   string
				projectID        string
				serviceID        string
			)
			err := tx.QueryRowContext(ctx,
				`SELECT a.healthy,
				        a.allocation_ip,
				        a.healthy_ports,
				        EXISTS(SELECT 1 FROM domain_bindings d WHERE d.service_id = a.service_id),
				        ag.workload_ipv6_subnet,
				        a.project_id,
				        a.service_id
				   FROM allocations a
				   JOIN agents ag ON ag.id = a.agent_id
				  WHERE a.id = $1 AND a.agent_id = $2`,
				cond.AllocationId, agentID,
			).Scan(&prevHealthy, &prevAllocationIP, (*jsonInt32Slice)(&prevHealthyPorts), &hasDomain, &workloadSubnet, &projectID, &serviceID)
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
			allocationIP, err := privateIPv6(workloadSubnet, projectID, serviceID)
			if err != nil {
				return fmt.Errorf("derive allocation ip: %w", err)
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
			if hasDomain && (prevHealthy != cond.Healthy || prevAllocationIP != allocationIP || !equalInt32Slices(prevHealthyPorts, cond.GetHealthyPorts())) {
				ingressChanged = true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return ingressChanged, nil
}

func (s *Store) validateAgentLogBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	type logOwner struct {
		allocationID string
		projectID    string
		serviceID    string
	}
	seen := make(map[logOwner]struct{}, len(batch.GetEntries()))
	for _, entry := range batch.GetEntries() {
		allocationID := entry.GetAllocationId()
		if allocationID == "" {
			continue
		}
		owner := logOwner{
			allocationID: allocationID,
			projectID:    entry.GetProjectId(),
			serviceID:    entry.GetServiceId(),
		}
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1
			   FROM allocations
			  WHERE id = $1 AND agent_id = $2 AND project_id = $3 AND service_id = $4`,
			allocationID, agentID, entry.GetProjectId(), entry.GetServiceId(),
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
		WireguardInterfaceName: s.mesh.InterfaceName,
		WireguardAddresses:     []string{agent.WireGuardIPv6},
		WireguardListenPort:    int32(agent.WireGuardListenPort),
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
		`SELECT s.id, s.project_id, s.name, s.current_spec_revision, s.current_rollout_generation, s.allocated_agent_id, s.created_at, s.updated_at
		   FROM services s
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
