package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Schema creates the usage tables. Usage rows have no foreign keys into the
// catalog, so catalog deletion never cascades into usage history.
var Schema = []string{
	// Attribution is captured when the control plane assigns a resource to an
	// agent. It outlives the catalog rows it copies.
	`CREATE TABLE usage_attributions (
		resource_kind TEXT NOT NULL CHECK (resource_kind IN ('allocation', 'volume')),
		resource_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		project_id TEXT NOT NULL,
		environment_id TEXT NOT NULL,
		service_id TEXT NOT NULL DEFAULT '',
		deployment_id TEXT NOT NULL DEFAULT '',
		environment_name TEXT NOT NULL,
		resource_name TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		ended_at TIMESTAMPTZ NULL,
		deleted_at TIMESTAMPTZ NULL,
		PRIMARY KEY (resource_kind, resource_id)
	)`,
	`CREATE INDEX idx_usage_attributions_project ON usage_attributions(project_id, resource_kind, resource_id)`,
	`CREATE INDEX idx_usage_attributions_current ON usage_attributions(agent_id) WHERE ended_at IS NULL`,
	`CREATE TABLE usage_producers (
		installation_id TEXT NOT NULL,
		producer_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('active', 'blocked', 'superseded')),
		accepted_through INT8 NOT NULL DEFAULT 0 CHECK (accepted_through >= 0),
		sealed_through TIMESTAMPTZ NULL,
		last_report_at TIMESTAMPTZ NULL,
		last_accepted_at TIMESTAMPTZ NULL,
		health JSONB NOT NULL DEFAULT '{}',
		conflict TEXT NOT NULL DEFAULT '',
		superseded_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (installation_id, producer_id)
	)`,
	`CREATE UNIQUE INDEX idx_usage_producers_agent ON usage_producers(installation_id, agent_id) WHERE state <> 'superseded'`,
	// Producer gaps cover sequences or time that no record can describe:
	// pruned records lost to a database restore, or a replaced journal.
	`CREATE TABLE usage_producer_gaps (
		installation_id TEXT NOT NULL,
		producer_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		first_sequence INT8 NOT NULL,
		last_sequence INT8 NOT NULL,
		start_at TIMESTAMPTZ NOT NULL,
		end_at TIMESTAMPTZ NOT NULL,
		reason TEXT NOT NULL,
		recorded_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (installation_id, producer_id, first_sequence, reason)
	)`,
	`CREATE INDEX idx_usage_producer_gaps_agent ON usage_producer_gaps(agent_id, start_at)`,
	// Progress keeps each resource's accepted intervals from overlapping, even
	// across producers and after raw partitions expire.
	`CREATE TABLE usage_resource_progress (
		resource_kind TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		end_at TIMESTAMPTZ NOT NULL,
		producer_id TEXT NOT NULL,
		sequence INT8 NOT NULL,
		PRIMARY KEY (resource_kind, resource_id)
	)`,
	`CREATE TABLE usage_intervals (
		installation_id TEXT NOT NULL,
		producer_id TEXT NOT NULL,
		sequence INT8 NOT NULL,
		payload_sha256 BYTEA NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('accepted', 'rejected')),
		rejection TEXT NOT NULL DEFAULT '',
		record_kind TEXT NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		counter_epoch TEXT NOT NULL,
		boot_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		project_id TEXT NOT NULL DEFAULT '',
		environment_id TEXT NOT NULL DEFAULT '',
		service_id TEXT NOT NULL DEFAULT '',
		start_at TIMESTAMPTZ NOT NULL,
		end_at TIMESTAMPTZ NOT NULL,
		duration_ms INT8 NOT NULL,
		meter_version TEXT NOT NULL,
		quality INT8 NOT NULL,
		cpu_usec NUMERIC(38,0) NOT NULL,
		memory_byte_ms NUMERIC(38,0) NOT NULL,
		memory_covered_ms INT8 NOT NULL,
		volume_byte_ms NUMERIC(38,0) NOT NULL,
		volume_covered_ms INT8 NOT NULL,
		retained BOOL NOT NULL,
		egress_public_direct NUMERIC(38,0) NOT NULL,
		egress_public_ingress NUMERIC(38,0) NOT NULL,
		egress_private NUMERIC(38,0) NOT NULL,
		egress_platform NUMERIC(38,0) NOT NULL,
		egress_probe NUMERIC(38,0) NOT NULL,
		gap_reason TEXT NOT NULL,
		payload BYTEA NULL,
		accepted_at TIMESTAMPTZ NOT NULL
	) PARTITION BY RANGE (start_at)`,
	`CREATE INDEX idx_usage_intervals_producer ON usage_intervals(installation_id, producer_id, sequence)`,
	`CREATE INDEX idx_usage_intervals_project ON usage_intervals(project_id, start_at)`,
	`CREATE INDEX idx_usage_intervals_resource ON usage_intervals(resource_id, start_at)`,
	`CREATE TABLE usage_intervals_default PARTITION OF usage_intervals DEFAULT`,
	// Hourly aggregates are derived from accepted intervals and can be rebuilt
	// from them while raw partitions are retained.
	`CREATE TABLE usage_hourly (
		hour TIMESTAMPTZ NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		project_id TEXT NOT NULL,
		environment_id TEXT NOT NULL,
		service_id TEXT NOT NULL,
		records INT8 NOT NULL,
		measured_ms INT8 NOT NULL,
		gap_ms INT8 NOT NULL,
		uncertain_ms INT8 NOT NULL,
		cpu_usec NUMERIC(38,0) NOT NULL,
		memory_byte_ms NUMERIC(38,0) NOT NULL,
		memory_covered_ms INT8 NOT NULL,
		volume_byte_ms NUMERIC(38,0) NOT NULL,
		volume_covered_ms INT8 NOT NULL,
		retained_volume_byte_ms NUMERIC(38,0) NOT NULL,
		egress_public_direct NUMERIC(38,0) NOT NULL,
		egress_public_ingress NUMERIC(38,0) NOT NULL,
		egress_private NUMERIC(38,0) NOT NULL,
		egress_platform NUMERIC(38,0) NOT NULL,
		egress_probe NUMERIC(38,0) NOT NULL,
		first_start TIMESTAMPTZ NOT NULL,
		last_end TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (hour, resource_kind, resource_id)
	)`,
	`CREATE INDEX idx_usage_hourly_project ON usage_hourly(project_id, hour)`,
}

// partitionsAhead is how many future UTC days keep a raw partition ready.
const partitionsAhead = 7

func partitionName(day time.Time) string {
	return "usage_intervals_" + day.UTC().Format("20060102")
}

// EnsurePartitions creates daily raw partitions from yesterday through
// partitionsAhead days after now. A day whose rows already landed in the
// default partition keeps them there; retention prunes both alike.
func EnsurePartitions(ctx context.Context, tx *sql.Tx, now time.Time) error {
	today := now.UTC().Truncate(24 * time.Hour)
	for offset := -1; offset <= partitionsAhead; offset++ {
		day := today.AddDate(0, 0, offset)
		name := partitionName(day)
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		var stranded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_intervals_default WHERE start_at >= $1 AND start_at < $2)`, day, day.AddDate(0, 0, 1)).Scan(&stranded); err != nil {
			return err
		}
		if stranded {
			continue
		}
		statement := fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_intervals FOR VALUES FROM ('%s') TO ('%s')`,
			name, day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create usage partition %s: %w", name, err)
		}
	}
	return nil
}

// dropExpiredPartitions drops whole daily partitions that end at or before
// cutoff and deletes expired rows from the default partition.
func dropExpiredPartitions(ctx context.Context, tx *sql.Tx, cutoff time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'usage_intervals'::regclass AND c.relname LIKE 'usage_intervals_2%'`)
	if err != nil {
		return 0, err
	}
	var expired []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "usage_intervals_"))
		if err != nil {
			continue
		}
		if !day.AddDate(0, 0, 1).After(cutoff) {
			expired = append(expired, name)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, name := range expired {
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+name); err != nil {
			return 0, fmt.Errorf("drop usage partition %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_intervals_default WHERE start_at < $1`, cutoff); err != nil {
		return 0, err
	}
	return len(expired), nil
}
