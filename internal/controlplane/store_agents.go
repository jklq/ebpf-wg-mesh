package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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

func (s *Store) validateWorkloadIPv4Pool(ctx context.Context) error {
	return s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		pool := s.mesh.WorkloadIPv4PoolCIDR
		prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
		if strings.TrimSpace(pool) == "" || prefixBits == 0 {
			return errors.New("IPv4 workload pool and per-node prefix size are required")
		}
		if _, err := deliverycore.Ipv4SubnetAt(pool, prefixBits, 0); err != nil {
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
				if deliverycore.Ipv4PrefixesOverlap(prefix, other) {
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
		if _, err := deliverycore.Ipv4SubnetAt(pool, prefixBits, uint64(nextOrdinal)); err != nil && len(allocated) == 0 && nextOrdinal == 0 {
			return err
		}
		return nil
	})
}

func (s *Store) schedulerSnapshot(ctx context.Context) ([]deliverycore.AgentRecord, []deliverycore.ServiceRecord, error) {
	return s.deliveryQueries().SchedulerSnapshotTx(ctx, s.db)
}
