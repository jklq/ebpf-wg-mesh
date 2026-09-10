package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

func (s *persistence) listAgents(ctx context.Context) ([]AgentRecord, error) {
	return s.listAgentsQuerier(ctx, s.db)
}

func (s *persistence) agentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	return agentByIDQuerier(ctx, s.db, agentID, false)
}

func (s *persistence) agentIDs(ctx context.Context) ([]string, error) {
	return s.agentIDsQuerier(ctx, s.db)
}

func (s *persistence) agentIDsQuerier(ctx context.Context, q ServiceQueryer) ([]string, error) {
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

func (s *persistence) allocateWorkloadSubnetTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
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

func (s *persistence) allocateWorkloadIPv4SubnetTx(ctx context.Context, tx *sql.Tx) (string, error) {
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

func (s *persistence) allocateWorkloadIPv4AddressTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	var subnet string
	if err := tx.QueryRowContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&subnet); err != nil {
		return "", err
	}
	if strings.TrimSpace(subnet) == "" {
		return "", fmt.Errorf("agent %s has no IPv4 workload prefix", agentID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT allocation_ipv4 FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> $2 AND allocation_ipv4 <> ''`, agentID, AllocationRolloutLost)
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

func (d *Delivery) backfillWorkloadIPv4AddressesTx(ctx context.Context, tx *sql.Tx, agentID, subnet string, now time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, allocation_ipv4 FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> $2 ORDER BY created_at ASC, id ASC FOR UPDATE`, agentID, AllocationRolloutLost)
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
	decisions := make([]SchedulingDecision, 0, len(missing))
	for _, allocationID := range missing {
		address, err := nextIPv4AddressFromSubnet(subnet, used)
		if err != nil {
			return false, err
		}
		decisions = append(decisions, SchedulingDecision{Kind: DecisionReserveAddress, AllocationID: allocationID, Allocation: AllocationAssignment{IPv4: address}})
		used[address] = struct{}{}
	}
	if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, decisions...)); err != nil {
		return false, err
	}
	return len(missing) > 0, nil
}

func (s *persistence) allocateWireGuardIPv6Tx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
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

func (s *persistence) schedulerSnapshotTx(ctx context.Context, q ServiceQueryer) ([]AgentRecord, []ServiceRecord, error) {
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

	var services []ServiceRecord
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

func (s *persistence) listAgentsQuerier(ctx context.Context, q ServiceQueryer) ([]AgentRecord, error) {
	rows, err := q.QueryContext(ctx,
		agentSelectSQL+`
		  ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentRecord
	for rows.Next() {
		rec, err := scanAgentRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
