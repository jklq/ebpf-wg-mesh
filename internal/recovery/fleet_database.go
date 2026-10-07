package recovery

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
)

// CheckEmptyDestination runs before any platform or console schema initializer.
// Cockroach's built-in defaultdb/postgres databases may exist, but contain no
// user tables. Never drop an occupied destination to make a restore fit.
func CheckEmptyDestination(ctx context.Context, db *sql.DB) error {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM [SHOW DATABASES] WHERE database_name NOT IN ('system','defaultdb','postgres')`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("full-cluster restore destination contains user-created databases")
	}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM [SHOW TABLES FROM defaultdb]) + (SELECT count(*) FROM [SHOW TABLES FROM postgres])`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("full-cluster restore destination contains user-created tables")
	}
	return nil
}

// ReadDesiredFleet reads a consistent snapshot from the restored database;
// operator input may supply observations, never desired allocation state.
func ReadDesiredFleet(ctx context.Context, db *sql.DB) ([]FleetHost, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, local_store_id, workload_ipv4_subnet, workload_ipv6_subnet, wireguard_ipv6 FROM agent_registrations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var hosts []FleetHost
	index := map[string]int{}
	for rows.Next() {
		var h FleetHost
		var ipv4, ipv6, wg string
		if err := rows.Scan(&h.ID, &h.LocalStoreID, &ipv4, &ipv6, &wg); err != nil {
			rows.Close()
			return nil, err
		}
		for _, prefix := range []string{ipv4, ipv6} {
			if prefix != "" {
				h.Reservations = append(h.Reservations, NetworkReservation{Owner: h.ID, Prefix: prefix})
			}
		}
		if wg != "" {
			prefix, err := netip.ParsePrefix(wg)
			if err != nil {
				rows.Close()
				return nil, err
			}
			h.Reservations = append(h.Reservations, NetworkReservation{Owner: h.ID, Prefix: netip.PrefixFrom(prefix.Addr(), prefix.Addr().BitLen()).String()})
		}
		index[h.ID] = len(hosts)
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT a.agent_id, a.id, a.service_id, s.environment_id, COALESCE(a.deployment_id,''), a.desired_spec_revision, a.desired_rollout_generation, a.allocation_ipv4, a.allocation_ipv6, a.created_at, e.network_identity
		FROM allocation_assignments a JOIN services s ON s.id = a.service_id JOIN environments e ON e.id = s.environment_id ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a FleetAllocation
		var agentID string
		var identity uint32
		if err := rows.Scan(&agentID, &a.ID, &a.ServiceID, &a.EnvironmentID, &a.DeploymentID, &a.SpecRevision, &a.RolloutGeneration, &a.IPv4, &a.IPv6, &a.CreatedAt, &identity); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := index[agentID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("desired allocation has no registered agent")
		}
		hosts[i].Allocations = append(hosts[i].Allocations, a)
		hosts[i].Reservations = append(hosts[i].Reservations, NetworkReservation{Owner: agentID, EnvironmentID: a.EnvironmentID, Identity: identity})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return hosts, nil
}

// ReserveFleet is deliberately additive. Missing resources and unreachable
// machines never release reservations. Explicit resolution owns that action.
func ReserveFleet(ctx context.Context, db *sql.DB, report FleetReport) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, reservation := range report.Reservations {
		if reservation.Prefix != "" {
			if _, err := netip.ParsePrefix(reservation.Prefix); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_network_reservations(generation,owner,environment_id,network_identity,prefix)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, report.Generation, reservation.Owner, reservation.EnvironmentID, int64(reservation.Identity), reservation.Prefix); err != nil {
			return err
		}
		if reservation.Identity != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE environment_network_identity_counter SET next_identity = greatest(next_identity,$1) WHERE id = TRUE`, int64(reservation.Identity)+1); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// RecoveryPrefixAvailable protects both exact addresses and overlapping ranges.
func RecoveryPrefixAvailable(ctx context.Context, tx *sql.Tx, prefix netip.Prefix, owner string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT owner,prefix FROM recovery_network_reservations WHERE prefix <> ''`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var heldBy, raw string
		if err := rows.Scan(&heldBy, &raw); err != nil {
			return false, err
		}
		held, err := netip.ParsePrefix(raw)
		if err != nil {
			return false, err
		}
		if held.Overlaps(prefix) && !(heldBy == owner && held.Bits() < prefix.Bits()) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func ReadDesiredResources(ctx context.Context, db *sql.DB) ([]FleetResource, error) {
	rows, err := db.QueryContext(ctx, `SELECT 'volume',id,created_at FROM volumes UNION ALL SELECT 'service',id,created_at FROM services UNION ALL SELECT 'deployment',id,created_at FROM deployments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var resources []FleetResource
	for rows.Next() {
		var r FleetResource
		if err := rows.Scan(&r.Kind, &r.ID, &r.CreatedAt); err != nil {
			return nil, err
		}
		resources = append(resources, r)
	}
	return resources, rows.Err()
}
