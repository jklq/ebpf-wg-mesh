package logs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

const (
	defaultLogQueryLimit = 500
	maxLogQueryLimit     = 5000
	maxLogGapResults     = 500
	maxEntriesPerBatch   = 2000

	// MinProjectRetentionDays and MaxProjectRetentionDays bound
	// per-project log retention. Zero on a project means the platform
	// default.
	MinProjectRetentionDays = 1
	MaxProjectRetentionDays = 90
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

// ProjectRetention is the resolved tenant policy for one service.
type ProjectRetention struct {
	ProjectID     string
	RetentionDays int
}

// ProjectResolver maps service IDs to their tenant retention policy.
// The control plane injects a CockroachDB-backed implementation; a nil
// resolver keeps the platform default for every service.
type ProjectResolver func(ctx context.Context, serviceIDs []string) (map[string]ProjectRetention, error)

type LogStore struct {
	db                   *sql.DB
	resolve              ProjectResolver
	defaultRetentionDays int
}

type ServiceLog struct {
	ObservedAt        time.Time
	ProjectID         string
	EnvironmentID     string
	ServiceID         string
	AllocationID      string
	AgentID           string
	Stream            string
	RolloutGeneration int64
	Sequence          uint64
	LineID            string
	Line              string
	LogType           string
	BuildID           string
	Stage             string
	Event             string
	Attributes        map[string]string
	Truncated         bool
}

type ServiceLogGap struct {
	AllocationID string
	BuildID      string
	LogType      string
	Stream       string
	DroppedCount uint64
	Reason       string
	WindowStart  time.Time
	WindowEnd    time.Time
}

// ServiceLogPage is one authorized read: lines oldest-first with a
// forward cursor, plus the explicit gaps overlapping the range.
type ServiceLogPage struct {
	Lines         []ServiceLog
	Gaps          []ServiceLogGap
	NextPageToken string
}

type LogLineInput struct {
	ID                string
	ObservedAt        time.Time
	ExpiresAt         time.Time
	ProjectID         string
	EnvironmentID     string
	ServiceID         string
	AllocationID      string
	AgentID           string
	Stream            string
	LogType           LogType
	BuildID           string
	Stage             string
	Event             string
	Attributes        map[string]string
	Truncated         bool
	RolloutGeneration int64
	Sequence          uint64
	Line              string
}

// GapInput is one persisted drop window. ProjectID and ExpiresAt are
// normally resolved from the service's retention policy; callers
// override them only when they already know the tenant.
type GapInput struct {
	ProjectID    string
	ServiceID    string
	AllocationID string
	BuildID      string
	LogType      LogType
	Stream       string
	WindowStart  time.Time
	WindowEnd    time.Time
	DroppedCount uint64
	Reason       string
	Reporter     string
	// SummaryID is the producer's stable identity for a coalesced
	// drop lineage. When set, it keys the gap row so retried reports
	// replace their row even after their totals grew.
	SummaryID string
	ExpiresAt time.Time
}

func logStoreSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS service_logs (
	observed_at DateTime64(9, 'UTC'),
	ingested_at DateTime64(9, 'UTC'),
	expires_at DateTime,
	project_id String,
	environment_id String,
	service_id String,
	allocation_id String,
	agent_id String,
	stream LowCardinality(String),
	rollout_generation Int64,
	sequence UInt64,
	line_id String,
	line String,
	log_type LowCardinality(String),
	build_id String DEFAULT '',
	stage LowCardinality(String) DEFAULT '',
	event LowCardinality(String) DEFAULT '',
	attributes Map(String, String),
	truncated UInt8 DEFAULT 0
) ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(observed_at)
ORDER BY (service_id, observed_at, line_id)
TTL expires_at`,
		`CREATE TABLE IF NOT EXISTS service_log_gaps (
	service_id String,
	project_id String,
	allocation_id String,
	build_id String,
	log_type LowCardinality(String),
	stream LowCardinality(String),
	window_start DateTime64(9, 'UTC'),
	window_end DateTime64(9, 'UTC'),
	dropped_count UInt64,
	reason LowCardinality(String),
	reporter LowCardinality(String),
	gap_id String,
	ingested_at DateTime64(9, 'UTC'),
	expires_at DateTime
) ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(window_start)
ORDER BY (service_id, gap_id)
TTL expires_at`,
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
	store := &LogStore{db: db, defaultRetentionDays: cfg.RetentionDays}
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// SetProjectResolver injects tenant retention resolution. It must be
// called before serving traffic; it is not safe for concurrent use
// with writes.
func (s *LogStore) SetProjectResolver(resolve ProjectResolver) {
	if s == nil {
		return
	}
	s.resolve = resolve
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

func (s *LogStore) ensureSchema(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	var engine string
	err := s.db.QueryRowContext(ctx,
		`SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = 'service_logs'`,
	).Scan(&engine)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect log table engine: %w", err)
	}
	if engine != "" && !strings.Contains(engine, "ReplacingMergeTree") {
		// Clean cutover from the pre-2.9 MergeTree schema, which has
		// no line identity, no tenant attribution, and no per-row
		// retention. Telemetry is not customer authority, so the old
		// rows are dropped rather than carried forward with
		// incompatible semantics.
		slog.WarnContext(ctx, "dropping legacy log table for durable bounded logs cutover", "engine", engine)
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS service_logs`); err != nil {
			return fmt.Errorf("drop legacy log table: %w", err)
		}
	}
	for i, stmt := range logStoreSchema() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply log schema statement %d: %w", i, err)
		}
	}
	return nil
}

// WriteAgentBatch converts one agent batch already scoped onto its
// allocation owners into durable line and gap inputs. Ownership
// scoping stays with the caller; this method enforces size caps,
// truncates defensively, resolves tenant retention, and persists
// both lines and producer drop reports. Batches larger than
// maxEntriesPerBatch are trimmed with the tail counted as ingest
// gaps per affected service and allocation.
func (s *LogStore) WriteAgentBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	if !s.Enabled() || batch == nil {
		return nil
	}
	inputs, gaps := convertAgentBatch(agentID, batch)
	// A producer total without a breakdown cannot be attributed to a
	// service, so it only feeds the dropped counter, never a gap row.
	if err := s.WriteLogLines(ctx, inputs); err != nil {
		return err
	}
	return s.WriteGaps(ctx, gaps)
}

// convertAgentBatch maps one scoped agent batch onto durable line
// and gap inputs. Ownership scoping stays with the caller.
func convertAgentBatch(agentID string, batch *agentv1.LogBatch) ([]LogLineInput, []GapInput) {
	entries := batch.GetEntries()
	var trimmed []*agentv1.LogEntry
	if len(entries) > maxEntriesPerBatch {
		trimmed = entries[maxEntriesPerBatch:]
		entries = entries[:maxEntriesPerBatch]
	}
	inputs := make([]LogLineInput, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if strings.TrimSpace(entry.GetEnvironmentId()) == "" ||
			strings.TrimSpace(entry.GetServiceId()) == "" ||
			strings.TrimSpace(entry.GetAllocationId()) == "" {
			continue
		}
		line, truncated := logpipeline.TruncateLine(entry.GetLine())
		inputs = append(inputs, LogLineInput{
			ID:                strings.TrimSpace(entry.GetLineId()),
			ObservedAt:        entry.GetObservedAt().AsTime(),
			EnvironmentID:     entry.GetEnvironmentId(),
			ServiceID:         entry.GetServiceId(),
			AllocationID:      entry.GetAllocationId(),
			AgentID:           agentID,
			Stream:            entry.GetStream(),
			LogType:           logTypeFromProto(entry.GetLogType()),
			BuildID:           entry.GetBuildId(),
			Stage:             entry.GetStage(),
			Event:             entry.GetEvent(),
			Attributes:        entry.GetAttributes(),
			Truncated:         truncated || entry.GetTruncated(),
			RolloutGeneration: entry.GetRolloutGeneration(),
			Sequence:          entry.GetSequence(),
			Line:              line,
		})
	}
	gaps := dropSummariesToGaps(batch.GetDrops(), agentID)
	// The trimmed tail keeps its own attribution: each affected
	// service and allocation gets its own gap, never a single gap
	// pinned to the first entry.
	now := time.Now().UTC()
	type trimKey struct{ serviceID, allocationID, logType, stream string }
	trimmedCounts := make(map[trimKey]uint64)
	for _, entry := range trimmed {
		if entry == nil || strings.TrimSpace(entry.GetServiceId()) == "" {
			continue
		}
		trimmedCounts[trimKey{
			serviceID:    entry.GetServiceId(),
			allocationID: entry.GetAllocationId(),
			logType:      string(logTypeFromProto(entry.GetLogType())),
			stream:       normalizeLogStream(entry.GetStream()),
		}]++
	}
	for key, count := range trimmedCounts {
		gaps = append(gaps, GapInput{
			ServiceID:    key.serviceID,
			AllocationID: key.allocationID,
			LogType:      LogType(key.logType),
			Stream:       key.stream,
			WindowStart:  now,
			WindowEnd:    now,
			DroppedCount: count,
			Reason:       logpipeline.ReasonIngestOverflow,
			Reporter:     agentID,
		})
	}
	return inputs, gaps
}

func (s *LogStore) WriteLogLines(ctx context.Context, inputs []LogLineInput) error {
	if !s.Enabled() || len(inputs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	resolved, err := s.resolveProjects(ctx, inputs)
	if err != nil {
		return err
	}
	values := make([]string, 0, len(inputs))
	args := make([]any, 0, len(inputs)*19)
	for _, in := range inputs {
		if strings.TrimSpace(in.ServiceID) == "" {
			continue
		}
		observedAt := in.ObservedAt
		if observedAt.IsZero() {
			observedAt = now
		}
		observedAt = observedAt.UTC()
		line, truncated := logpipeline.TruncateLine(in.Line)
		id := strings.TrimSpace(in.ID)
		if id == "" {
			id = logpipeline.SyntheticLineID()
		}
		projectID := strings.TrimSpace(in.ProjectID)
		expiresAt := in.ExpiresAt
		if policy, ok := resolved[in.ServiceID]; ok {
			if projectID == "" {
				projectID = policy.ProjectID
			}
			if expiresAt.IsZero() {
				expiresAt = now.AddDate(0, 0, clampRetentionDays(policy.RetentionDays, s.defaultRetentionDays))
			}
		}
		if expiresAt.IsZero() {
			expiresAt = now.AddDate(0, 0, clampRetentionDays(0, s.defaultRetentionDays))
		}
		attrs := logpipeline.NormalizeAttributes(in.Attributes)
		if attrs == nil {
			attrs = map[string]string{}
		}
		var truncatedFlag uint8
		if truncated || in.Truncated {
			truncatedFlag = 1
		}
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			observedAt,
			now,
			expiresAt.UTC(),
			projectID,
			in.EnvironmentID,
			in.ServiceID,
			in.AllocationID,
			in.AgentID,
			normalizeLogStream(in.Stream),
			in.RolloutGeneration,
			in.Sequence,
			id,
			line,
			string(normalizeLogType(in.LogType)),
			in.BuildID,
			normalizeStageName(in.Stage),
			logpipeline.NormalizeEvent(in.Event),
			attrs,
			truncatedFlag,
		)
	}
	if len(values) == 0 {
		return nil
	}
	query := `INSERT INTO service_logs (
	observed_at,
	ingested_at,
	expires_at,
	project_id,
	environment_id,
	service_id,
	allocation_id,
	agent_id,
	stream,
	rollout_generation,
	sequence,
	line_id,
	line,
	log_type,
	build_id,
	stage,
	event,
	attributes,
	truncated
) VALUES ` + strings.Join(values, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert clickhouse log batch: %w", err)
	}
	return nil
}

// WriteGaps persists drop windows. Gap identity derives from the
// window itself so retried reports collapse instead of double
// counting.
func (s *LogStore) WriteGaps(ctx context.Context, gaps []GapInput) error {
	if !s.Enabled() || len(gaps) == 0 {
		return nil
	}
	now := time.Now().UTC()
	serviceIDs := make([]string, 0, len(gaps))
	seen := make(map[string]struct{}, len(gaps))
	for _, gap := range gaps {
		if gap.ServiceID == "" {
			continue
		}
		if _, ok := seen[gap.ServiceID]; !ok {
			seen[gap.ServiceID] = struct{}{}
			serviceIDs = append(serviceIDs, gap.ServiceID)
		}
	}
	var resolved map[string]ProjectRetention
	if s.resolve != nil && len(serviceIDs) > 0 {
		var err error
		resolved, err = s.resolve(ctx, serviceIDs)
		if err != nil {
			return fmt.Errorf("resolve log retention: %w", err)
		}
	}
	values := make([]string, 0, len(gaps))
	args := make([]any, 0, len(gaps)*14)
	for _, gap := range gaps {
		if gap.ServiceID == "" || gap.DroppedCount == 0 {
			continue
		}
		windowStart := gap.WindowStart.UTC()
		if windowStart.IsZero() {
			windowStart = now
		}
		windowEnd := gap.WindowEnd.UTC()
		if windowEnd.IsZero() || windowEnd.Before(windowStart) {
			windowEnd = windowStart
		}
		projectID := strings.TrimSpace(gap.ProjectID)
		expiresAt := gap.ExpiresAt
		if policy, ok := resolved[gap.ServiceID]; ok {
			if projectID == "" {
				projectID = policy.ProjectID
			}
			if expiresAt.IsZero() {
				expiresAt = now.AddDate(0, 0, clampRetentionDays(policy.RetentionDays, s.defaultRetentionDays))
			}
		}
		if expiresAt.IsZero() {
			expiresAt = now.AddDate(0, 0, clampRetentionDays(0, s.defaultRetentionDays))
		}
		logType := normalizeLogType(gap.LogType)
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			gap.ServiceID,
			projectID,
			gap.AllocationID,
			gap.BuildID,
			string(logType),
			normalizeLogStream(gap.Stream),
			windowStart,
			windowEnd,
			gap.DroppedCount,
			logpipeline.NormalizeDropReason(gap.Reason),
			normalizeReporter(gap.Reporter),
			gapIdentity(gap.ServiceID, gap.AllocationID, gap.BuildID, string(logType), gap.Stream, gap.Reason, gap.Reporter, gap.SummaryID, windowStart, windowEnd, gap.DroppedCount),
			now,
			expiresAt.UTC(),
		)
	}
	if len(values) == 0 {
		return nil
	}
	query := `INSERT INTO service_log_gaps (
	service_id,
	project_id,
	allocation_id,
	build_id,
	log_type,
	stream,
	window_start,
	window_end,
	dropped_count,
	reason,
	reporter,
	gap_id,
	ingested_at,
	expires_at
) VALUES ` + strings.Join(values, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert clickhouse log gaps: %w", err)
	}
	return nil
}

func (s *LogStore) resolveProjects(ctx context.Context, inputs []LogLineInput) (map[string]ProjectRetention, error) {
	if s.resolve == nil {
		return nil, nil
	}
	serviceIDs := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, in := range inputs {
		if in.ServiceID == "" || strings.TrimSpace(in.ProjectID) != "" && !in.ExpiresAt.IsZero() {
			continue
		}
		if _, ok := seen[in.ServiceID]; ok {
			continue
		}
		seen[in.ServiceID] = struct{}{}
		serviceIDs = append(serviceIDs, in.ServiceID)
	}
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	resolved, err := s.resolve(ctx, serviceIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve log retention: %w", err)
	}
	return resolved, nil
}

func clampRetentionDays(projectDays, platformDefault int) int {
	if projectDays > 0 {
		if projectDays < MinProjectRetentionDays {
			return MinProjectRetentionDays
		}
		if projectDays > MaxProjectRetentionDays {
			return MaxProjectRetentionDays
		}
		return projectDays
	}
	if platformDefault <= 0 {
		return 14
	}
	if platformDefault > MaxProjectRetentionDays {
		return MaxProjectRetentionDays
	}
	return platformDefault
}

// gapIdentity keys one gap row for ReplacingMergeTree dedup. Gaps
// carrying a stable producer summary ID key on that ID plus the drop
// identity: retries of the same coalesced lineage replace their row
// even after the reported totals or window grew. Internally derived
// gaps have no producer ID and are immutable per event, so they key
// on their full content.
func gapIdentity(serviceID, allocationID, buildID, logType, stream, reason, reporter, summaryID string, windowStart, windowEnd time.Time, count uint64) string {
	payload := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d",
		serviceID, allocationID, buildID, logType, stream,
		logpipeline.NormalizeDropReason(reason), reporter,
		windowStart.UnixNano(), windowEnd.UnixNano(), count)
	if summaryID != "" {
		payload = fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00sid\x00%s",
			serviceID, allocationID, buildID, logType, stream,
			logpipeline.NormalizeDropReason(reason), reporter, summaryID)
	}
	sum := sha256.Sum256([]byte(payload))
	return "gap:" + hex.EncodeToString(sum[:16])
}

// PurgeProjectLogs deletes every line and gap attributed to a
// destroyed project. Mutations run synchronously so a returned nil
// means the rows are gone; per-row TTL expiry remains the backstop
// if a purge is lost.
func (s *LogStore) PurgeProjectLogs(ctx context.Context, projectID string) error {
	if !s.Enabled() {
		return nil
	}
	if strings.TrimSpace(projectID) == "" {
		return errors.New("project id is required")
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE service_logs DELETE WHERE project_id = ? SETTINGS mutations_sync = 1`, projectID); err != nil {
		return fmt.Errorf("purge project logs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE service_log_gaps DELETE WHERE project_id = ? SETTINGS mutations_sync = 1`, projectID); err != nil {
		return fmt.Errorf("purge project log gaps: %w", err)
	}
	return nil
}

// ListServiceLogs returns one authorized page, oldest first, with the
// gaps overlapping the queried range. Callers must authorize the
// service before calling.
func (s *LogStore) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (ServiceLogPage, error) {
	var page ServiceLogPage
	if !s.Enabled() {
		return page, ErrDisabled
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	if limit > maxLogQueryLimit {
		limit = maxLogQueryLimit
	}
	cursorTime, cursorID, err := logpipeline.DecodeCursor(req.GetPageToken())
	if err != nil {
		return page, fmt.Errorf("invalid page token: %w", err)
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
	if req.GetPageToken() != "" {
		filters = append(filters, "(observed_at, line_id) > (?, ?)")
		args = append(args, cursorTime, cursorID)
	}
	if search := strings.TrimSpace(req.GetSearch()); search != "" {
		filters = append(filters, "positionCaseInsensitive(line, ?) > 0")
		args = append(args, search)
	}
	args = append(args, limit+1)
	query := `
SELECT observed_at, project_id, environment_id, service_id, allocation_id, agent_id, stream, rollout_generation, sequence, line_id, line, log_type, build_id, stage, event, attributes, truncated
  FROM service_logs FINAL
 WHERE ` + strings.Join(filters, " AND ") + `
 ORDER BY observed_at ASC, line_id ASC
 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, fmt.Errorf("query clickhouse service logs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rec ServiceLog
		var truncated uint8
		if err := rows.Scan(
			&rec.ObservedAt,
			&rec.ProjectID,
			&rec.EnvironmentID,
			&rec.ServiceID,
			&rec.AllocationID,
			&rec.AgentID,
			&rec.Stream,
			&rec.RolloutGeneration,
			&rec.Sequence,
			&rec.LineID,
			&rec.Line,
			&rec.LogType,
			&rec.BuildID,
			&rec.Stage,
			&rec.Event,
			&rec.Attributes,
			&truncated,
		); err != nil {
			return page, fmt.Errorf("scan clickhouse service log: %w", err)
		}
		rec.Truncated = truncated != 0
		page.Lines = append(page.Lines, rec)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("iterate clickhouse service logs: %w", err)
	}
	if len(page.Lines) > limit {
		last := page.Lines[limit-1]
		page.NextPageToken = logpipeline.EncodeCursor(last.ObservedAt, last.LineID)
		page.Lines = page.Lines[:limit]
	}
	gaps, err := s.listGaps(ctx, req)
	if err != nil {
		return page, err
	}
	page.Gaps = gaps
	return page, nil
}

func (s *LogStore) listGaps(ctx context.Context, req *platformv1.ListServiceLogsRequest) ([]ServiceLogGap, error) {
	filters := []string{"service_id = ?"}
	args := []any{req.GetServiceId()}
	if allocationID := strings.TrimSpace(req.GetAllocationId()); allocationID != "" {
		filters = append(filters, "(allocation_id = ? OR allocation_id = '')")
		args = append(args, allocationID)
	}
	if buildID := strings.TrimSpace(req.GetBuildId()); buildID != "" {
		filters = append(filters, "(build_id = ? OR build_id = '')")
		args = append(args, buildID)
	}
	if logType := logTypeFromProto(req.GetLogType()); logType != "" {
		filters = append(filters, "log_type = ?")
		args = append(args, string(logType))
	}
	if start := req.GetStartTime(); start != nil {
		filters = append(filters, "window_end >= ?")
		args = append(args, start.AsTime().UTC())
	}
	if end := req.GetEndTime(); end != nil {
		filters = append(filters, "window_start <= ?")
		args = append(args, end.AsTime().UTC())
	}
	args = append(args, maxLogGapResults)
	query := `
SELECT allocation_id, build_id, log_type, stream, dropped_count, reason, window_start, window_end
  FROM service_log_gaps FINAL
 WHERE ` + strings.Join(filters, " AND ") + `
 ORDER BY window_start ASC
 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query clickhouse log gaps: %w", err)
	}
	defer rows.Close()
	var gaps []ServiceLogGap
	for rows.Next() {
		var gap ServiceLogGap
		if err := rows.Scan(
			&gap.AllocationID,
			&gap.BuildID,
			&gap.LogType,
			&gap.Stream,
			&gap.DroppedCount,
			&gap.Reason,
			&gap.WindowStart,
			&gap.WindowEnd,
		); err != nil {
			return nil, fmt.Errorf("scan clickhouse log gap: %w", err)
		}
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clickhouse log gaps: %w", err)
	}
	return gaps, nil
}

func dropSummariesToGaps(drops []*platformv1.LogDropSummary, reporter string) []GapInput {
	var gaps []GapInput
	for _, drop := range drops {
		if drop == nil || drop.GetDroppedCount() == 0 || strings.TrimSpace(drop.GetServiceId()) == "" {
			continue
		}
		gaps = append(gaps, GapInput{
			ServiceID:    drop.GetServiceId(),
			AllocationID: drop.GetAllocationId(),
			BuildID:      drop.GetBuildId(),
			LogType:      logTypeFromProto(drop.GetLogType()),
			Stream:       drop.GetStream(),
			WindowStart:  drop.GetWindowStart().AsTime(),
			WindowEnd:    drop.GetWindowEnd().AsTime(),
			DroppedCount: drop.GetDroppedCount(),
			Reason:       drop.GetReason(),
			Reporter:     reporter,
			SummaryID:    drop.GetSummaryId(),
		})
	}
	return gaps
}

// DropSummariesToGaps converts producer drop reports into gap inputs
// for non-agent producers such as builders.
func DropSummariesToGaps(drops []*platformv1.LogDropSummary, reporter string) []GapInput {
	return dropSummariesToGaps(drops, reporter)
}

func normalizeReporter(reporter string) string {
	reporter = strings.TrimSpace(reporter)
	if reporter == "" {
		return "controlplane"
	}
	if len(reporter) > 128 {
		return reporter[:128]
	}
	return reporter
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
	if len(stage) > logpipeline.MaxStageNameBytes {
		stage = stage[:logpipeline.MaxStageNameBytes]
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
