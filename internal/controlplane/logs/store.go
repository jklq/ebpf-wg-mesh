package logs

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

type LogType string

const (
	LogTypeRuntime LogType = "runtime"
	LogTypeBuild   LogType = "build"
	LogTypeDeploy  LogType = "deploy"
	LogTypeHTTP    LogType = "http"
	LogTypeNetwork LogType = "network"
)

var ErrDisabled = errors.New("log storage is not configured")

type LogStore struct {
	db *sql.DB
}

type ServiceLog struct {
	ObservedAt        time.Time
	EnvironmentID     string
	ServiceID         string
	AllocationID      string
	AgentID           string
	Stream            string
	RolloutGeneration int64
	Sequence          uint64
	Line              string
	LogType           string
	BuildID           string
	Stage             string
}

type LogLineInput struct {
	ObservedAt        time.Time
	EnvironmentID     string
	ServiceID         string
	AllocationID      string
	AgentID           string
	Stream            string
	LogType           LogType
	BuildID           string
	Stage             string
	RolloutGeneration int64
	Sequence          uint64
	Line              string
}

type logStoreMigration struct {
	name  string
	stmts []string
}

func logStoreMigrations(retentionDays int) []logStoreMigration {
	if retentionDays <= 0 {
		retentionDays = 14
	}
	return []logStoreMigration{
		{
			name: "service_logs_table",
			stmts: []string{
				fmt.Sprintf(`CREATE TABLE IF NOT EXISTS service_logs (
	observed_at DateTime64(9, 'UTC'),
	ingested_at DateTime64(9, 'UTC'),
	environment_id String,
	service_id String,
	allocation_id String,
	agent_id String,
	stream LowCardinality(String),
	rollout_generation Int64,
	sequence UInt64,
	line String,
	log_type LowCardinality(String) DEFAULT 'runtime',
	build_id String DEFAULT '',
	stage LowCardinality(String) DEFAULT ''
) ENGINE = MergeTree
PARTITION BY toYYYYMM(observed_at)
ORDER BY (environment_id, service_id, log_type, observed_at, allocation_id, sequence)
TTL toDateTime(observed_at) + INTERVAL %d DAY`, retentionDays),
			},
		},
	}
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

func (s *LogStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *LogStore) Enabled() bool {
	return s != nil && s.db != nil
}

func (s *LogStore) Ready(ctx context.Context) bool {
	if !s.Enabled() {
		return true
	}
	return s.db.PingContext(ctx) == nil
}

func (s *LogStore) ensureSchema(ctx context.Context, retentionDays int) error {
	if !s.Enabled() {
		return nil
	}
	for _, migration := range logStoreMigrations(retentionDays) {
		for _, stmt := range migration.stmts {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply log migration %s: %w", migration.name, err)
			}
		}
	}
	return nil
}

func (s *LogStore) WriteAgentBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	if !s.Enabled() || batch == nil || len(batch.GetEntries()) == 0 {
		return nil
	}
	inputs := make([]LogLineInput, 0, len(batch.GetEntries()))
	for _, entry := range batch.GetEntries() {
		if strings.TrimSpace(entry.GetEnvironmentId()) == "" ||
			strings.TrimSpace(entry.GetServiceId()) == "" ||
			strings.TrimSpace(entry.GetAllocationId()) == "" {
			continue
		}
		observedAt := entry.GetObservedAt().AsTime()
		inputs = append(inputs, LogLineInput{
			ObservedAt:        observedAt,
			EnvironmentID:     entry.GetEnvironmentId(),
			ServiceID:         entry.GetServiceId(),
			AllocationID:      entry.GetAllocationId(),
			AgentID:           agentID,
			Stream:            entry.GetStream(),
			LogType:           logTypeFromProto(entry.GetLogType()),
			BuildID:           entry.GetBuildId(),
			Stage:             entry.GetStage(),
			RolloutGeneration: entry.GetRolloutGeneration(),
			Sequence:          entry.GetSequence(),
			Line:              entry.GetLine(),
		})
	}
	return s.WriteLogLines(ctx, inputs)
}

func (s *LogStore) WriteLogLines(ctx context.Context, inputs []LogLineInput) error {
	if !s.Enabled() || len(inputs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	values := make([]string, 0, len(inputs))
	args := make([]any, 0, len(inputs)*13)
	for _, in := range inputs {
		if strings.TrimSpace(in.EnvironmentID) == "" || strings.TrimSpace(in.ServiceID) == "" {
			continue
		}
		observedAt := in.ObservedAt
		if observedAt.IsZero() {
			observedAt = now
		}
		logType := normalizeLogType(in.LogType)
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			observedAt.UTC(),
			now,
			in.EnvironmentID,
			in.ServiceID,
			in.AllocationID,
			in.AgentID,
			normalizeLogStream(in.Stream),
			in.RolloutGeneration,
			in.Sequence,
			in.Line,
			string(logType),
			in.BuildID,
			normalizeStageName(in.Stage),
		)
	}
	if len(values) == 0 {
		return nil
	}
	query := `INSERT INTO service_logs (
	observed_at,
	ingested_at,
	environment_id,
	service_id,
	allocation_id,
	agent_id,
	stream,
	rollout_generation,
	sequence,
	line,
	log_type,
	build_id,
	stage
) VALUES ` + strings.Join(values, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert clickhouse log batch: %w", err)
	}
	return nil
}

func (s *LogStore) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) ([]ServiceLog, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	if limit > maxLogQueryLimit {
		limit = maxLogQueryLimit
	}
	filters := []string{"service_id = ?"}
	args := []any{req.GetServiceId()}
	if allocationID := strings.TrimSpace(req.GetAllocationId()); allocationID != "" {
		filters = append(filters, "allocation_id = ?")
		args = append(args, allocationID)
	}
	if buildID := strings.TrimSpace(req.GetBuildId()); buildID != "" {
		filters = append(filters, "build_id = ?")
		args = append(args, buildID)
	}
	if logType := logTypeFromProto(req.GetLogType()); logType != "" {
		filters = append(filters, "log_type = ?")
		args = append(args, string(logType))
	}
	if start := req.GetStartTime(); start != nil {
		filters = append(filters, "observed_at >= ?")
		args = append(args, start.AsTime().UTC())
	}
	if end := req.GetEndTime(); end != nil {
		filters = append(filters, "observed_at <= ?")
		args = append(args, end.AsTime().UTC())
	}
	if search := strings.TrimSpace(req.GetSearch()); search != "" {
		filters = append(filters, "positionCaseInsensitive(line, ?) > 0")
		args = append(args, search)
	}
	args = append(args, limit)
	query := `
SELECT observed_at, environment_id, service_id, allocation_id, agent_id, stream, rollout_generation, sequence, line, log_type, build_id, stage
  FROM service_logs
 WHERE ` + strings.Join(filters, " AND ") + `
 ORDER BY observed_at DESC, sequence DESC
 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query clickhouse service logs: %w", err)
	}
	defer rows.Close()

	var newestFirst []ServiceLog
	for rows.Next() {
		var rec ServiceLog
		if err := rows.Scan(
			&rec.ObservedAt,
			&rec.EnvironmentID,
			&rec.ServiceID,
			&rec.AllocationID,
			&rec.AgentID,
			&rec.Stream,
			&rec.RolloutGeneration,
			&rec.Sequence,
			&rec.Line,
			&rec.LogType,
			&rec.BuildID,
			&rec.Stage,
		); err != nil {
			return nil, fmt.Errorf("scan clickhouse service log: %w", err)
		}
		newestFirst = append(newestFirst, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clickhouse service logs: %w", err)
	}
	out := make([]ServiceLog, len(newestFirst))
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

func normalizeLogType(t LogType) LogType {
	switch LogType(strings.TrimSpace(strings.ToLower(string(t)))) {
	case LogTypeBuild:
		return LogTypeBuild
	case LogTypeDeploy:
		return LogTypeDeploy
	case LogTypeHTTP:
		return LogTypeHTTP
	case LogTypeNetwork:
		return LogTypeNetwork
	case LogTypeRuntime:
		return LogTypeRuntime
	default:
		return LogTypeRuntime
	}
}

func normalizeStageName(stage string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return ""
	}
	return strings.ToLower(stage)
}

func logTypeFromProto(t platformv1.ServiceLogType) LogType {
	switch t {
	case platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME:
		return LogTypeRuntime
	case platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD:
		return LogTypeBuild
	case platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY:
		return LogTypeDeploy
	case platformv1.ServiceLogType_SERVICE_LOG_TYPE_HTTP:
		return LogTypeHTTP
	case platformv1.ServiceLogType_SERVICE_LOG_TYPE_NETWORK:
		return LogTypeNetwork
	default:
		return ""
	}
}

func TypeToProto(t string) platformv1.ServiceLogType {
	switch LogType(t) {
	case LogTypeRuntime:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME
	case LogTypeBuild:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD
	case LogTypeDeploy:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY
	case LogTypeHTTP:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_HTTP
	case LogTypeNetwork:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_NETWORK
	default:
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME
	}
}
