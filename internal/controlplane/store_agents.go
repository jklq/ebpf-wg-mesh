package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

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
	return s.agentIDsQuerier(ctx, s.db)
}

func (s *Store) agentIDsQuerier(ctx context.Context, q serviceQueryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM agents ORDER BY created_at ASC`)
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
