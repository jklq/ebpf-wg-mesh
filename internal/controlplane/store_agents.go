package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

// scopeAgentLogBatch resolves the authoritative owner of every
// allocation referenced by one agent log batch and scopes the batch
// onto it. Claimed service and environment IDs must match the
// allocation owner when present; empty claims (drop summaries whose
// producer metadata is gone after a restart) are filled from the
// owner, so a compromised or buggy agent cannot attribute output or
// loss to another tenant. Lines referencing an allocation this agent
// does not own — including a stale one removed while its lines sat in
// the durable spool — or claiming a mismatched owner are excluded
// individually with a warning: one bad line must never reject the
// whole durable batch and wedge delivery behind it, and an
// unverifiable line cannot carry a gap row either.
// Entries and drop summaries without an allocation cannot be
// attributed and are removed.
func (s *fleetPersistence) scopeAgentLogBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	wanted := make(map[string]struct{}, len(batch.GetEntries())+len(batch.GetDrops()))
	for _, entry := range batch.GetEntries() {
		if id := entry.GetAllocationId(); id != "" {
			wanted[id] = struct{}{}
		}
	}
	for _, drop := range batch.GetDrops() {
		if id := drop.GetAllocationId(); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		batch.Entries = nil
		batch.Drops = nil
		return nil
	}
	ids := make([]string, 0, len(wanted))
	placeholders := make([]string, 0, len(wanted))
	args := make([]any, 0, len(wanted)+1)
	args = append(args, agentID)
	for id := range wanted {
		ids = append(ids, id)
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	type logOwner struct {
		environmentID string
		serviceID     string
	}
	owners := make(map[string]logOwner, len(ids))
	rows, err := s.db.QueryContext(ctx,
		`SELECT allocations.id, allocations.service_id, s.environment_id
		   FROM allocations
		   JOIN services s ON s.id = allocations.service_id
		  WHERE allocations.agent_id = $1
		    AND allocations.id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, serviceID, environmentID string
		if err := rows.Scan(&id, &serviceID, &environmentID); err != nil {
			return err
		}
		owners[id] = logOwner{environmentID: environmentID, serviceID: serviceID}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keptEntries := batch.Entries[:0]
	excluded := 0
	for _, entry := range batch.Entries {
		owner, ok := owners[entry.GetAllocationId()]
		if !ok {
			excluded++
			continue
		}
		if claimed := strings.TrimSpace(entry.GetServiceId()); claimed != "" && claimed != owner.serviceID {
			excluded++
			slog.Warn("dropped agent log line with mismatched service claim", "agent_id", agentID, "allocation_id", entry.GetAllocationId(), "claimed_service_id", claimed)
			continue
		}
		if claimed := strings.TrimSpace(entry.GetEnvironmentId()); claimed != "" && claimed != owner.environmentID {
			excluded++
			slog.Warn("dropped agent log line with mismatched environment claim", "agent_id", agentID, "allocation_id", entry.GetAllocationId(), "claimed_environment_id", claimed)
			continue
		}
		entry.ServiceId = owner.serviceID
		entry.EnvironmentId = owner.environmentID
		keptEntries = append(keptEntries, entry)
	}
	batch.Entries = keptEntries
	keptDrops := batch.Drops[:0]
	for _, drop := range batch.Drops {
		owner, ok := owners[drop.GetAllocationId()]
		if !ok {
			excluded++
			continue
		}
		if claimed := strings.TrimSpace(drop.GetServiceId()); claimed != "" && claimed != owner.serviceID {
			excluded++
			slog.Warn("dropped agent drop summary with mismatched service claim", "agent_id", agentID, "allocation_id", drop.GetAllocationId(), "claimed_service_id", claimed)
			continue
		}
		drop.ServiceId = owner.serviceID
		keptDrops = append(keptDrops, drop)
	}
	batch.Drops = keptDrops
	if excluded > 0 {
		slog.Warn("excluded unattributable agent log lines", "agent_id", agentID, "count", excluded)
	}
	return nil
}

func (s *fleetPersistence) validateWorkloadIPv4Pool(ctx context.Context) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		pool := s.mesh.WorkloadIPv4PoolCIDR
		prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
		if strings.TrimSpace(pool) == "" || prefixBits == 0 {
			return errors.New("IPv4 workload pool and per-node prefix size are required")
		}
		if _, err := deliverycore.IPv4SubnetAt(pool, prefixBits, 0); err != nil {
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
				if deliverycore.IPv4PrefixesOverlap(prefix, other) {
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
		return nil
	})
}
