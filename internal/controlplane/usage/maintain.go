package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
)

// Retention reports what one maintenance pass removed.
type Retention struct {
	DroppedPartitions int
	HourlyRows        int64
	Attributions      int64
}

// Maintain prepares upcoming raw partitions and applies retention. Raw
// intervals expire after Config.RawRetention and hourly aggregates after
// Config.HourlyRetention. Producer cursors never expire, so a replayed
// record stays a duplicate after its raw row is gone. Attribution and
// overlap progress of resources that ended before the hourly retention
// window are removed with them.
func (s *Store) Maintain(ctx context.Context) (Retention, error) {
	var result Retention
	err := sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		result = Retention{}
		var now time.Time
		if err := tx.QueryRowContext(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if err := EnsurePartitions(ctx, tx, now); err != nil {
			return err
		}
		dropped, err := dropExpiredPartitions(ctx, tx, now.Add(-s.cfg.RawRetention))
		if err != nil {
			return err
		}
		result.DroppedPartitions = dropped
		hourlyCutoff := now.Add(-s.cfg.HourlyRetention)
		deleted, err := tx.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour < $1`, hourlyCutoff)
		if err != nil {
			return err
		}
		result.HourlyRows, _ = deleted.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_producer_gaps WHERE end_at < $1`, hourlyCutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_resource_progress p USING usage_attributions a
			WHERE a.resource_kind = p.resource_kind AND a.resource_id = p.resource_id AND a.ended_at < $1`, hourlyCutoff); err != nil {
			return err
		}
		deleted, err = tx.ExecContext(ctx, `DELETE FROM usage_attributions WHERE ended_at < $1`, hourlyCutoff)
		if err != nil {
			return err
		}
		result.Attributions, _ = deleted.RowsAffected()
		return nil
	})
	return result, err
}

// RebuildHourly recomputes hourly aggregates for whole hours in [from, to)
// from accepted raw intervals. It refuses hours whose raw partitions may
// already have expired, because the rebuild would then lose usage.
func (s *Store) RebuildHourly(ctx context.Context, from, to time.Time) error {
	from, to = from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var now time.Time
		if err := tx.QueryRowContext(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if from.Before(now.Add(-s.cfg.RawRetention).Add(24 * time.Hour)) {
			return fmt.Errorf("rebuild from %s reaches past raw usage retention", from.Format(time.RFC3339))
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour >= $1 AND hour < $2`, from, to); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_hourly(
				hour, resource_kind, resource_id, project_id, environment_id, service_id,
				records, measured_ms, gap_ms, uncertain_ms, cpu_usec, memory_byte_ms, memory_covered_ms,
				volume_byte_ms, volume_covered_ms, retained_volume_byte_ms,
				egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe,
				first_start, last_end)
			SELECT date_trunc('hour', start_at, 'UTC'), resource_kind, resource_id,
				min(project_id), min(environment_id), min(service_id),
				count(*),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'measurement'), 0),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'gap'), 0),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'measurement' AND quality & 1 <> 0), 0),
				sum(cpu_usec), sum(memory_byte_ms), sum(memory_covered_ms),
				sum(volume_byte_ms), sum(volume_covered_ms),
				COALESCE(sum(volume_byte_ms) FILTER (WHERE retained), 0),
				sum(egress_public_direct), sum(egress_public_ingress), sum(egress_private), sum(egress_platform), sum(egress_probe),
				min(start_at), max(end_at)
			  FROM usage_intervals
			 WHERE status = 'accepted' AND start_at >= $1 AND start_at < $2
			 GROUP BY 1, 2, 3`, from, to)
		return err
	})
}
