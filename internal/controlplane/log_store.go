package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

const (
	defaultLogQueryLimit = 500
	maxLogQueryLimit     = 5000
)

var errLogStoreDisabled = errors.New("log storage is not configured")

type LogStore struct {
	db *sql.DB
}

type serviceLogRecord struct {
	ObservedAt        time.Time
	ProjectID         string
	ServiceID         string
	AllocationID      string
	AgentID           string
	Stream            string
	RolloutGeneration int64
	Sequence          uint64
	Line              string
}

func OpenLogStore(ctx context.Context, cfg config.LogCaptureConfig) (*LogStore, error) {
	if strings.TrimSpace(cfg.ClickHouse.URL) == "" {
		return nil, nil
	}
	normalizeLogCaptureConfig(&cfg)
	db, err := sql.Open("clickhouse", cfg.ClickHouse.URL)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	db.SetMaxOpenConns(cfg.ClickHouse.MaxOpenConns)
	db.SetMaxIdleConns(cfg.ClickHouse.MaxIdleConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	store := &LogStore{db: db}
	if err := store.ensureSchema(ctx, cfg.RetentionDays); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func normalizeLogCaptureConfig(cfg *config.LogCaptureConfig) {
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 14
	}
	if cfg.ClickHouse.MaxOpenConns <= 0 {
		cfg.ClickHouse.MaxOpenConns = 8
	}
	if cfg.ClickHouse.MaxIdleConns <= 0 {
		cfg.ClickHouse.MaxIdleConns = cfg.ClickHouse.MaxOpenConns
	}
	if cfg.ClickHouse.MaxIdleConns > cfg.ClickHouse.MaxOpenConns {
		cfg.ClickHouse.MaxIdleConns = cfg.ClickHouse.MaxOpenConns
	}
}

func serviceLogsSchema(retentionDays int) string {
	if retentionDays <= 0 {
		retentionDays = 14
	}
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS service_logs (
	observed_at DateTime64(9, 'UTC'),
	ingested_at DateTime64(9, 'UTC'),
	project_id String,
	service_id String,
	allocation_id String,
	agent_id String,
	stream LowCardinality(String),
	rollout_generation Int64,
	sequence UInt64,
	line String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(observed_at)
ORDER BY (project_id, service_id, observed_at, allocation_id, sequence)
TTL toDateTime(observed_at) + INTERVAL %d DAY`, retentionDays)
}

func (s *LogStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *LogStore) Enabled() bool {
	return s != nil && s.db != nil
}

func (s *LogStore) ensureSchema(ctx context.Context, retentionDays int) error {
	if !s.Enabled() {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, serviceLogsSchema(retentionDays)); err != nil {
		return fmt.Errorf("create clickhouse service_logs schema: %w", err)
	}
	return nil
}

func (s *LogStore) WriteAgentBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	if !s.Enabled() || batch == nil || len(batch.GetEntries()) == 0 {
		return nil
	}
	now := time.Now().UTC()
	values := make([]string, 0, len(batch.GetEntries()))
	args := make([]any, 0, len(batch.GetEntries())*10)
	for _, entry := range batch.GetEntries() {
		if strings.TrimSpace(entry.GetProjectId()) == "" ||
			strings.TrimSpace(entry.GetServiceId()) == "" ||
			strings.TrimSpace(entry.GetAllocationId()) == "" {
			continue
		}
		observedAt := entry.GetObservedAt().AsTime()
		if observedAt.IsZero() {
			observedAt = now
		}
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			observedAt.UTC(),
			now,
			entry.GetProjectId(),
			entry.GetServiceId(),
			entry.GetAllocationId(),
			agentID,
			normalizeLogStream(entry.GetStream()),
			entry.GetRolloutGeneration(),
			entry.GetSequence(),
			entry.GetLine(),
		)
	}
	if len(values) == 0 {
		return nil
	}
	query := `
INSERT INTO service_logs (
	observed_at,
	ingested_at,
	project_id,
	service_id,
	allocation_id,
	agent_id,
	stream,
	rollout_generation,
	sequence,
	line
) VALUES ` + strings.Join(values, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert clickhouse log batch: %w", err)
	}
	return nil
}

func (s *LogStore) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) ([]serviceLogRecord, error) {
	if !s.Enabled() {
		return nil, errLogStoreDisabled
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	if limit > maxLogQueryLimit {
		limit = maxLogQueryLimit
	}
	filters := []string{"project_id = ?", "service_id = ?"}
	args := []any{req.GetProjectId(), req.GetServiceId()}
	if allocationID := strings.TrimSpace(req.GetAllocationId()); allocationID != "" {
		filters = append(filters, "allocation_id = ?")
		args = append(args, allocationID)
	}
	if start := req.GetStartTime(); start != nil {
		filters = append(filters, "observed_at >= ?")
		args = append(args, start.AsTime().UTC())
	}
	if end := req.GetEndTime(); end != nil {
		filters = append(filters, "observed_at <= ?")
		args = append(args, end.AsTime().UTC())
	}
	args = append(args, limit)
	query := `
SELECT observed_at, project_id, service_id, allocation_id, agent_id, stream, rollout_generation, sequence, line
  FROM service_logs
 WHERE ` + strings.Join(filters, " AND ") + `
 ORDER BY observed_at DESC, sequence DESC
 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query clickhouse service logs: %w", err)
	}
	defer rows.Close()

	var newestFirst []serviceLogRecord
	for rows.Next() {
		var rec serviceLogRecord
		if err := rows.Scan(
			&rec.ObservedAt,
			&rec.ProjectID,
			&rec.ServiceID,
			&rec.AllocationID,
			&rec.AgentID,
			&rec.Stream,
			&rec.RolloutGeneration,
			&rec.Sequence,
			&rec.Line,
		); err != nil {
			return nil, fmt.Errorf("scan clickhouse service log: %w", err)
		}
		newestFirst = append(newestFirst, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clickhouse service logs: %w", err)
	}
	out := make([]serviceLogRecord, len(newestFirst))
	for i := range newestFirst {
		out[len(newestFirst)-1-i] = newestFirst[i]
	}
	return out, nil
}

func normalizeLogStream(stream string) string {
	switch strings.TrimSpace(strings.ToLower(stream)) {
	case "stdout":
		return "stdout"
	case "stderr":
		return "stderr"
	default:
		return "combined"
	}
}
