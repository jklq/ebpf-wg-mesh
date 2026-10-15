package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/sqlretry"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrInvalidReport is a malformed report envelope. The session must end;
	// a well-formed producer never sends one.
	ErrInvalidReport = errors.New("invalid usage report")
	// ErrStaleSession fences reports from a superseded agent session.
	ErrStaleSession = errors.New("usage report from a stale agent session")
	// ErrProducerSuperseded rejects a journal that a newer journal replaced
	// for the same agent.
	ErrProducerSuperseded = errors.New("usage producer was superseded by a newer journal")
	// ErrForeignProducer rejects a producer bound to another agent.
	ErrForeignProducer = errors.New("usage producer belongs to another agent")
)

// Config bounds retention and freshness.
type Config struct {
	RawRetention    time.Duration
	HourlyRetention time.Duration
	// StaleAfter marks query results stale when a current resource's
	// producer has not sealed usage for this long.
	StaleAfter time.Duration
}

func (c Config) withDefaults() Config {
	if c.RawRetention <= 0 {
		c.RawRetention = 30 * 24 * time.Hour
	}
	if c.HourlyRetention <= 0 {
		c.HourlyRetention = 90 * 24 * time.Hour
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 90 * time.Second
	}
	return c
}

// Store ingests, retains, and reads usage in PostgreSQL.
type Store struct {
	db  *sql.DB
	cfg Config
}

func New(db *sql.DB, cfg Config) *Store {
	return &Store{db: db, cfg: cfg.withDefaults()}
}

// Producer is the authenticated sender of a report: the agent from the
// verified client certificate and the session incarnation it registered.
type Producer struct {
	InstallationID     string
	AgentID            string
	SessionIncarnation uint64
}

// Ingest durably accepts a report in one transaction and returns the
// acknowledgement to send after commit. Records continue the producer's
// contiguous cursor; duplicates are no-ops, and a different payload under
// an accepted identity blocks the producer.
func (s *Store) Ingest(ctx context.Context, producer Producer, report *agentv1.UsageReport) (*agentv1.UsageAcknowledgement, error) {
	if err := checkEnvelope(producer, report); err != nil {
		return nil, err
	}
	var ack *agentv1.UsageAcknowledgement
	err := sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		var err error
		ack, err = ingestTx(ctx, tx, producer, report)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ack, nil
}

func checkEnvelope(producer Producer, report *agentv1.UsageReport) error {
	if report.GetAgentId() != producer.AgentID || report.GetInstallationId() != producer.InstallationID {
		return fmt.Errorf("%w: agent or installation differs from the session", ErrInvalidReport)
	}
	if _, err := uuid.Parse(report.GetProducerId()); err != nil {
		return fmt.Errorf("%w: producer_id must be a UUID", ErrInvalidReport)
	}
	if len(report.GetRecords()) > agentv1.MaxUsageRecordsPerReport || proto.Size(report) > agentv1.MaxUsageReportBytes {
		return fmt.Errorf("%w: report exceeds %d records or %d bytes", ErrInvalidReport, agentv1.MaxUsageRecordsPerReport, agentv1.MaxUsageReportBytes)
	}
	for i, record := range report.GetRecords() {
		if record.GetInstallationId() != report.GetInstallationId() || record.GetProducerId() != report.GetProducerId() {
			return fmt.Errorf("%w: record %d names another producer", ErrInvalidReport, record.GetSequence())
		}
		if record.GetSequence() == 0 || i > 0 && record.GetSequence() != report.GetRecords()[i-1].GetSequence()+1 {
			return fmt.Errorf("%w: record sequences must be positive and contiguous", ErrInvalidReport)
		}
	}
	return nil
}

type producerRow struct {
	agentID         string
	state           string
	acceptedThrough uint64
	sealedThrough   sql.NullTime
	conflict        string
}

func ingestTx(ctx context.Context, tx *sql.Tx, producer Producer, report *agentv1.UsageReport) (*agentv1.UsageAcknowledgement, error) {
	ack := &agentv1.UsageAcknowledgement{ProducerId: report.GetProducerId(), ReportId: report.GetReportId()}
	now, limits, err := fenceSession(ctx, tx, producer)
	if err != nil {
		return nil, err
	}
	current, err := lockProducer(ctx, tx, producer, report, now)
	if err != nil {
		return nil, err
	}
	if current.state == "blocked" {
		ack.AcceptedThrough, ack.Conflict = current.acceptedThrough, current.conflict
		return ack, recordHealth(ctx, tx, report, now)
	}
	cursor := current.acceptedThrough
	if lost := report.GetLostThrough(); lost > cursor {
		end := now
		if records := report.GetRecords(); len(records) > 0 && records[0].GetStart().AsTime().Before(now) {
			end = records[0].GetStart().AsTime()
		}
		start := end
		if current.sealedThrough.Valid && current.sealedThrough.Time.Before(end) {
			start = current.sealedThrough.Time
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_producer_gaps(installation_id, producer_id, agent_id, first_sequence, last_sequence, start_at, end_at, reason, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'acknowledged_records_lost', $8) ON CONFLICT DO NOTHING`,
			producer.InstallationID, report.GetProducerId(), producer.AgentID, int64(cursor+1), int64(lost), start, end, now); err != nil {
			return nil, err
		}
		cursor = lost
	}

	var duplicates, fresh []*agentv1.UsageRecord
	for _, record := range report.GetRecords() {
		switch {
		case record.GetSequence() <= cursor:
			duplicates = append(duplicates, record)
		case record.GetSequence() == cursor+uint64(len(fresh))+1:
			fresh = append(fresh, record)
		}
	}
	if conflict, err := checkDuplicates(ctx, tx, producer.InstallationID, report.GetProducerId(), duplicates); err != nil {
		return nil, err
	} else if conflict != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE usage_producers SET state = 'blocked', conflict = $3, accepted_through = $4, updated_at = $5
			WHERE installation_id = $1 AND producer_id = $2`, producer.InstallationID, report.GetProducerId(), conflict, int64(cursor), now); err != nil {
			return nil, err
		}
		ack.AcceptedThrough, ack.Conflict = cursor, conflict
		return ack, recordHealth(ctx, tx, report, now)
	}

	batch, err := prepareBatch(ctx, tx, producer, fresh, now, limits)
	if err != nil {
		return nil, err
	}
	if err := batch.write(ctx, tx); err != nil {
		return nil, err
	}
	cursor += uint64(len(fresh))
	ack.AcceptedThrough = cursor
	ack.Rejections = batch.rejections

	complete := len(report.GetRecords()) == 0 || report.GetRecords()[len(report.GetRecords())-1].GetSequence() <= cursor
	var sealed any
	if complete && report.GetSealedThrough().IsValid() && !report.GetSealedThrough().AsTime().After(now.Add(futureSkew)) {
		sealed = report.GetSealedThrough().AsTime()
	}
	var acceptedAt any
	if len(fresh) > 0 {
		acceptedAt = now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_producers
		   SET accepted_through = $3,
		       sealed_through = CASE WHEN $4::TIMESTAMPTZ IS NULL THEN sealed_through ELSE GREATEST(COALESCE(sealed_through, $4::TIMESTAMPTZ), $4::TIMESTAMPTZ) END,
		       last_accepted_at = COALESCE($5::TIMESTAMPTZ, last_accepted_at),
		       updated_at = $6
		 WHERE installation_id = $1 AND producer_id = $2`,
		producer.InstallationID, report.GetProducerId(), int64(cursor), sealed, acceptedAt, now); err != nil {
		return nil, err
	}
	return ack, recordHealth(ctx, tx, report, now)
}

// fenceSession admits only the agent's current session and returns the
// database time and the agent's declared node resources.
func fenceSession(ctx context.Context, tx *sql.Tx, producer Producer) (time.Time, nodeLimits, error) {
	var now time.Time
	var incarnation int64
	var revoked sql.NullTime
	var state string
	var limits nodeLimits
	var memoryMiB int64
	err := tx.QueryRowContext(ctx, `SELECT statement_timestamp(), r.session_incarnation, ad.credential_revoked_at, ad.lifecycle_state,
			r.cpu_millis_capacity + r.reserved_cpu_millis, r.memory_mebibytes_capacity + r.reserved_memory_mebibytes
		  FROM agent_registrations r JOIN agent_administration ad ON ad.agent_id = r.id
		 WHERE r.id = $1 FOR SHARE OF r`, producer.AgentID).Scan(&now, &incarnation, &revoked, &state, &limits.cpuMillis, &memoryMiB)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nodeLimits{}, ErrStaleSession
	}
	if err != nil {
		return time.Time{}, nodeLimits{}, err
	}
	if uint64(incarnation) != producer.SessionIncarnation || revoked.Valid || state == "retired" {
		return time.Time{}, nodeLimits{}, ErrStaleSession
	}
	limits.memoryBytes = memoryMiB << 20
	return now.UTC(), limits, nil
}

// lockProducer locks the producer row, creating it for a new journal. A new
// journal supersedes the agent's previous one so the old journal cannot
// report again; the time between them becomes an explicit producer gap.
func lockProducer(ctx context.Context, tx *sql.Tx, producer Producer, report *agentv1.UsageReport, now time.Time) (producerRow, error) {
	var row producerRow
	var through int64
	err := tx.QueryRowContext(ctx, `SELECT agent_id, state, accepted_through, sealed_through, conflict
		  FROM usage_producers WHERE installation_id = $1 AND producer_id = $2 FOR UPDATE`,
		producer.InstallationID, report.GetProducerId()).Scan(&row.agentID, &row.state, &through, &row.sealedThrough, &row.conflict)
	row.acceptedThrough = uint64(through)
	if err == nil {
		switch {
		case row.agentID != producer.AgentID:
			return row, ErrForeignProducer
		case row.state == "superseded":
			return row, ErrProducerSuperseded
		}
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return row, err
	}
	var previous string
	var previousSealed sql.NullTime
	var previousCreated time.Time
	err = tx.QueryRowContext(ctx, `SELECT producer_id, sealed_through, created_at FROM usage_producers
		 WHERE installation_id = $1 AND agent_id = $2 AND state <> 'superseded' FOR UPDATE`,
		producer.InstallationID, producer.AgentID).Scan(&previous, &previousSealed, &previousCreated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return row, err
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE usage_producers SET state = 'superseded', superseded_by = $3, updated_at = $4
			WHERE installation_id = $1 AND producer_id = $2`, producer.InstallationID, previous, report.GetProducerId(), now); err != nil {
			return row, err
		}
		start := previousCreated
		if previousSealed.Valid {
			start = previousSealed.Time
		}
		end := now
		if records := report.GetRecords(); len(records) > 0 && records[0].GetStart().AsTime().Before(now) {
			end = records[0].GetStart().AsTime()
		}
		if end.Before(start) {
			end = start
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_producer_gaps(installation_id, producer_id, agent_id, first_sequence, last_sequence, start_at, end_at, reason, recorded_at)
			VALUES ($1, $2, $3, 0, 0, $4, $5, 'journal_replaced', $6) ON CONFLICT DO NOTHING`,
			producer.InstallationID, previous, producer.AgentID, start, end, now); err != nil {
			return row, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_producers(installation_id, producer_id, agent_id, state, created_at, updated_at)
		VALUES ($1, $2, $3, 'active', $4, $4)`, producer.InstallationID, report.GetProducerId(), producer.AgentID, now); err != nil {
		return row, err
	}
	return producerRow{agentID: producer.AgentID, state: "active"}, nil
}

// checkDuplicates compares retried records with the stored payload digest.
// A sequence whose raw row passed retention is still a duplicate: the
// producer cursor alone proves it was accepted.
func checkDuplicates(ctx context.Context, tx *sql.Tx, installationID, producerID string, records []*agentv1.UsageRecord) (string, error) {
	if len(records) == 0 {
		return "", nil
	}
	sequences := make([]int64, len(records))
	for i, record := range records {
		sequences[i] = int64(record.GetSequence())
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence, payload_sha256 FROM usage_intervals
		WHERE installation_id = $1 AND producer_id = $2 AND sequence = ANY($3)`, installationID, producerID, sequences)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	stored := make(map[uint64][]byte, len(records))
	for rows.Next() {
		var sequence int64
		var digest []byte
		if err := rows.Scan(&sequence, &digest); err != nil {
			return "", err
		}
		stored[uint64(sequence)] = digest
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, record := range records {
		digest, ok := stored[record.GetSequence()]
		if ok && string(digest) != string(agentv1.UsageRecordDigest(record)) {
			return fmt.Sprintf("sequence %d differs from the accepted record", record.GetSequence()), nil
		}
	}
	return "", nil
}

func recordHealth(ctx context.Context, tx *sql.Tx, report *agentv1.UsageReport, now time.Time) error {
	health := []byte("{}")
	if report.GetHealth() != nil {
		var err error
		if health, err = protojson.Marshal(report.GetHealth()); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_producers SET health = $3, last_report_at = $4
		WHERE installation_id = $1 AND producer_id = $2`, report.GetInstallationId(), report.GetProducerId(), health, now)
	return err
}

// attribution is the retained control-plane record that authorizes a
// resource for an agent.
type attribution struct {
	agentID, projectID, environmentID, serviceID string
	createdAt                                    time.Time
	deletedAt                                    sql.NullTime
}

type resourceKey struct{ kind, id string }

func resourceKind(kind agentv1.UsageResourceKind) string {
	if kind == agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME {
		return KindVolume
	}
	return KindAllocation
}

func recordKind(kind agentv1.UsageRecordKind) string {
	if kind == agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP {
		return "gap"
	}
	return "measurement"
}

func loadAttributions(ctx context.Context, tx *sql.Tx, records []*agentv1.UsageRecord) (map[resourceKey]attribution, error) {
	ids := uniqueResourceIDs(records)
	rows, err := tx.QueryContext(ctx, `SELECT a.resource_kind, a.resource_id, a.agent_id, a.project_id, a.environment_id, a.service_id, a.created_at,
			CASE WHEN a.resource_kind = 'volume' THEN COALESCE(
				(SELECT LEAST(v.deleted_at, e.deleted_at, p.deleted_at)
				   FROM volumes v JOIN environments e ON e.id = v.environment_id JOIN projects p ON p.id = e.project_id
				  WHERE v.id = a.resource_id),
				a.deleted_at) END
		  FROM usage_attributions a WHERE a.resource_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[resourceKey]attribution)
	for rows.Next() {
		var key resourceKey
		var a attribution
		if err := rows.Scan(&key.kind, &key.id, &a.agentID, &a.projectID, &a.environmentID, &a.serviceID, &a.createdAt, &a.deletedAt); err != nil {
			return nil, err
		}
		out[key] = a
	}
	return out, rows.Err()
}

func loadProgress(ctx context.Context, tx *sql.Tx, records []*agentv1.UsageRecord) (map[resourceKey]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT resource_kind, resource_id, end_at FROM usage_resource_progress
		WHERE resource_id = ANY($1) ORDER BY resource_kind, resource_id FOR UPDATE`, uniqueResourceIDs(records))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[resourceKey]time.Time)
	for rows.Next() {
		var key resourceKey
		var end time.Time
		if err := rows.Scan(&key.kind, &key.id, &end); err != nil {
			return nil, err
		}
		out[key] = end
	}
	return out, rows.Err()
}

func uniqueResourceIDs(records []*agentv1.UsageRecord) []string {
	seen := make(map[string]struct{}, len(records))
	var ids []string
	for _, record := range records {
		if _, ok := seen[record.GetResourceId()]; !ok {
			seen[record.GetResourceId()] = struct{}{}
			ids = append(ids, record.GetResourceId())
		}
	}
	return ids
}

// prepareBatch validates records in sequence order. Every record is stored:
// accepted records contribute to aggregates, and rejected records keep their
// payload and reason for operators.
func prepareBatch(ctx context.Context, tx *sql.Tx, producer Producer, records []*agentv1.UsageRecord, now time.Time, limits nodeLimits) (*batch, error) {
	b := &batch{producer: producer, now: now, hourly: make(map[hourKey]*hourly), progress: make(map[resourceKey]progress)}
	if len(records) == 0 {
		return b, nil
	}
	attributions, err := loadAttributions(ctx, tx, records)
	if err != nil {
		return nil, err
	}
	ends, err := loadProgress(ctx, tx, records)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		key := resourceKey{kind: resourceKind(record.GetResourceKind()), id: record.GetResourceId()}
		q, reason := validateRecord(record, now, limits)
		a, found := attributions[key]
		if reason == "" {
			start := record.GetStart().AsTime()
			switch {
			case !found:
				reason = RejectUnknownAttribution
			case a.agentID != producer.AgentID:
				reason = RejectForeignAgent
			case start.Before(a.createdAt.Add(-attributionSkew)):
				reason = RejectBeforeAttribution
			case start.Before(ends[key]):
				reason = RejectOverlap
			}
		}
		if reason != "" {
			b.reject(record, reason, a)
			continue
		}
		end := record.GetEnd().AsTime()
		ends[key] = end
		b.accept(record, key, q, a)
	}
	return b, nil
}

type progress struct {
	end      time.Time
	sequence uint64
	producer string
}

type hourKey struct {
	hour time.Time
	resourceKey
}

type hourly struct {
	projectID, environmentID, serviceID string
	records, measured, gap, uncertain   int64
	memoryCovered, volumeCovered        int64
	cpu, memory, volume, retained       big.Int
	egress                              [5]big.Int
	first, last                         time.Time
}

// batch accumulates column arrays for bulk statements.
type batch struct {
	producer   Producer
	now        time.Time
	rows       intervalColumns
	hourly     map[hourKey]*hourly
	progress   map[resourceKey]progress
	rejections []*agentv1.UsageRejection
}

type intervalColumns struct {
	installation, producerID, status, rejection, recordKind, resourceKind, resourceID, epoch, boot []string
	agent, project, environment, service, meter, gapReason                                         []string
	sequence, duration, quality, memoryCovered, volumeCovered                                      []int64
	digest, payload                                                                                [][]byte
	start, end                                                                                     []time.Time
	cpu, memory, volume, direct, ingress, private, platform, probe                                 []string
	retained                                                                                       []bool
}

func (b *batch) reject(record *agentv1.UsageRecord, reason string, a attribution) {
	payload, _ := proto.Marshal(record)
	start, end := b.now, b.now
	if record.GetStart().IsValid() && record.GetEnd().IsValid() && !record.GetEnd().AsTime().Before(record.GetStart().AsTime()) {
		start, end = record.GetStart().AsTime(), record.GetEnd().AsTime()
	}
	b.append(record, "rejected", reason, a, start, end, quantities{}, false, payload)
	b.rejections = append(b.rejections, &agentv1.UsageRejection{Sequence: record.GetSequence(), Reason: reason})
}

func (b *batch) accept(record *agentv1.UsageRecord, key resourceKey, q quantities, a attribution) {
	start, end := record.GetStart().AsTime().UTC(), record.GetEnd().AsTime().UTC()
	retained := key.kind == KindVolume && a.deletedAt.Valid && !start.Before(a.deletedAt.Time)
	b.append(record, "accepted", "", a, start, end, q, retained, nil)
	b.progress[key] = progress{end: end, sequence: record.GetSequence(), producer: record.GetProducerId()}

	hk := hourKey{hour: start.Truncate(time.Hour), resourceKey: key}
	h := b.hourly[hk]
	if h == nil {
		h = &hourly{projectID: a.projectID, environmentID: a.environmentID, serviceID: a.serviceID, first: start, last: end}
		b.hourly[hk] = h
	}
	h.records++
	duration := record.GetDurationMs()
	if record.GetKind() == agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP {
		h.gap += duration
	} else {
		h.measured += duration
		if agentv1.HasUsageQuality(record.GetQuality(), agentv1.UsageQuality_USAGE_QUALITY_UNCERTAIN_TIMING) {
			h.uncertain += duration
		}
	}
	h.memoryCovered += record.GetMemoryCoveredMs()
	h.volumeCovered += record.GetVolumeCoveredMs()
	if !q.zero() {
		h.cpu.Add(&h.cpu, q.cpu)
		h.memory.Add(&h.memory, q.memory)
		h.volume.Add(&h.volume, q.volume)
		if retained {
			h.retained.Add(&h.retained, q.volume)
		}
		for i := range q.egress {
			h.egress[i].Add(&h.egress[i], q.egress[i])
		}
	}
	if start.Before(h.first) {
		h.first = start
	}
	if end.After(h.last) {
		h.last = end
	}
}

func (b *batch) append(record *agentv1.UsageRecord, status, rejection string, a attribution, start, end time.Time, q quantities, retained bool, payload []byte) {
	c := &b.rows
	c.installation = append(c.installation, record.GetInstallationId())
	c.producerID = append(c.producerID, record.GetProducerId())
	c.sequence = append(c.sequence, int64(record.GetSequence()))
	c.digest = append(c.digest, agentv1.UsageRecordDigest(record))
	c.status = append(c.status, status)
	c.rejection = append(c.rejection, rejection)
	c.recordKind = append(c.recordKind, recordKind(record.GetKind()))
	c.resourceKind = append(c.resourceKind, resourceKind(record.GetResourceKind()))
	c.resourceID = append(c.resourceID, truncate(record.GetResourceId(), 128))
	c.epoch = append(c.epoch, truncate(record.GetCounterEpoch(), 160))
	c.boot = append(c.boot, truncate(record.GetBootId(), 64))
	c.agent = append(c.agent, b.producer.AgentID)
	if status == "accepted" {
		c.project = append(c.project, a.projectID)
		c.environment = append(c.environment, a.environmentID)
		c.service = append(c.service, a.serviceID)
	} else {
		c.project, c.environment, c.service = append(c.project, ""), append(c.environment, ""), append(c.service, "")
	}
	c.start = append(c.start, start)
	c.end = append(c.end, end)
	c.duration = append(c.duration, record.GetDurationMs())
	c.meter = append(c.meter, truncate(record.GetMeterVersion(), 32))
	c.quality = append(c.quality, int64(record.GetQuality()))
	c.cpu = append(c.cpu, decimal(q.cpu))
	c.memory = append(c.memory, decimal(q.memory))
	c.memoryCovered = append(c.memoryCovered, coverage(status, record.GetMemoryCoveredMs()))
	c.volume = append(c.volume, decimal(q.volume))
	c.volumeCovered = append(c.volumeCovered, coverage(status, record.GetVolumeCoveredMs()))
	c.retained = append(c.retained, retained)
	for i, column := range []*[]string{&c.direct, &c.ingress, &c.private, &c.platform, &c.probe} {
		*column = append(*column, decimal(q.egress[i]))
	}
	c.gapReason = append(c.gapReason, truncate(record.GetGapReason(), 64))
	c.payload = append(c.payload, payload)
}

func coverage(status string, value int64) int64 {
	if status != "accepted" {
		return 0
	}
	return value
}

func decimal(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func truncate(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func (b *batch) write(ctx context.Context, tx *sql.Tx) error {
	c := b.rows
	if len(c.sequence) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_intervals(
			installation_id, producer_id, sequence, payload_sha256, status, rejection, record_kind,
			resource_kind, resource_id, counter_epoch, boot_id, agent_id, project_id, environment_id, service_id,
			start_at, end_at, duration_ms, meter_version, quality,
			cpu_usec, memory_byte_ms, memory_covered_ms, volume_byte_ms, volume_covered_ms, retained,
			egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe,
			gap_reason, payload, accepted_at)
		SELECT installation_id, producer_id, sequence, payload_sha256, status, rejection, record_kind,
			resource_kind, resource_id, counter_epoch, boot_id, agent_id, project_id, environment_id, service_id,
			start_at, end_at, duration_ms, meter_version, quality,
			cpu, memory, memory_covered, volume, volume_covered, retained,
			direct, ingress, private, platform, probe, gap_reason, payload, $34
		  FROM unnest($1::TEXT[], $2::TEXT[], $3::INT8[], $4::BYTEA[], $5::TEXT[], $6::TEXT[], $7::TEXT[],
			$8::TEXT[], $9::TEXT[], $10::TEXT[], $11::TEXT[], $12::TEXT[], $13::TEXT[], $14::TEXT[], $15::TEXT[],
			$16::TIMESTAMPTZ[], $17::TIMESTAMPTZ[], $18::INT8[], $19::TEXT[], $20::INT8[],
			$21::NUMERIC[], $22::NUMERIC[], $23::INT8[], $24::NUMERIC[], $25::INT8[], $26::BOOL[],
			$27::NUMERIC[], $28::NUMERIC[], $29::NUMERIC[], $30::NUMERIC[], $31::NUMERIC[], $32::TEXT[], $33::BYTEA[])
		    AS r(installation_id, producer_id, sequence, payload_sha256, status, rejection, record_kind,
			resource_kind, resource_id, counter_epoch, boot_id, agent_id, project_id, environment_id, service_id,
			start_at, end_at, duration_ms, meter_version, quality,
			cpu, memory, memory_covered, volume, volume_covered, retained,
			direct, ingress, private, platform, probe, gap_reason, payload)`,
		c.installation, c.producerID, c.sequence, c.digest, c.status, c.rejection, c.recordKind,
		c.resourceKind, c.resourceID, c.epoch, c.boot, c.agent, c.project, c.environment, c.service,
		c.start, c.end, c.duration, c.meter, c.quality,
		c.cpu, c.memory, c.memoryCovered, c.volume, c.volumeCovered, c.retained,
		c.direct, c.ingress, c.private, c.platform, c.probe, c.gapReason, c.payload, b.now); err != nil {
		return fmt.Errorf("insert usage intervals: %w", err)
	}
	if err := b.writeHourly(ctx, tx); err != nil {
		return err
	}
	return b.writeProgress(ctx, tx)
}

func (b *batch) writeHourly(ctx context.Context, tx *sql.Tx) error {
	if len(b.hourly) == 0 {
		return nil
	}
	var (
		hours, first, last                                               []time.Time
		kinds, ids, projects, environments, services                     []string
		records, measured, gaps, uncertain, memoryCovered, volumeCovered []int64
		cpu, memory, volume, retained                                    []string
		egress                                                           [5][]string
	)
	for key, h := range b.hourly {
		hours, first, last = append(hours, key.hour), append(first, h.first), append(last, h.last)
		kinds, ids = append(kinds, key.kind), append(ids, key.id)
		projects, environments, services = append(projects, h.projectID), append(environments, h.environmentID), append(services, h.serviceID)
		records, measured, gaps, uncertain = append(records, h.records), append(measured, h.measured), append(gaps, h.gap), append(uncertain, h.uncertain)
		memoryCovered, volumeCovered = append(memoryCovered, h.memoryCovered), append(volumeCovered, h.volumeCovered)
		cpu, memory, volume, retained = append(cpu, h.cpu.String()), append(memory, h.memory.String()), append(volume, h.volume.String()), append(retained, h.retained.String())
		for i := range egress {
			egress[i] = append(egress[i], h.egress[i].String())
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_hourly AS h(
			hour, resource_kind, resource_id, project_id, environment_id, service_id,
			records, measured_ms, gap_ms, uncertain_ms, cpu_usec, memory_byte_ms, memory_covered_ms,
			volume_byte_ms, volume_covered_ms, retained_volume_byte_ms,
			egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe,
			first_start, last_end)
		SELECT * FROM unnest($1::TIMESTAMPTZ[], $2::TEXT[], $3::TEXT[], $4::TEXT[], $5::TEXT[], $6::TEXT[],
			$7::INT8[], $8::INT8[], $9::INT8[], $10::INT8[], $11::NUMERIC[], $12::NUMERIC[], $13::INT8[],
			$14::NUMERIC[], $15::INT8[], $16::NUMERIC[],
			$17::NUMERIC[], $18::NUMERIC[], $19::NUMERIC[], $20::NUMERIC[], $21::NUMERIC[],
			$22::TIMESTAMPTZ[], $23::TIMESTAMPTZ[])
		ON CONFLICT (hour, resource_kind, resource_id) DO UPDATE SET
			records = h.records + EXCLUDED.records,
			measured_ms = h.measured_ms + EXCLUDED.measured_ms,
			gap_ms = h.gap_ms + EXCLUDED.gap_ms,
			uncertain_ms = h.uncertain_ms + EXCLUDED.uncertain_ms,
			cpu_usec = h.cpu_usec + EXCLUDED.cpu_usec,
			memory_byte_ms = h.memory_byte_ms + EXCLUDED.memory_byte_ms,
			memory_covered_ms = h.memory_covered_ms + EXCLUDED.memory_covered_ms,
			volume_byte_ms = h.volume_byte_ms + EXCLUDED.volume_byte_ms,
			volume_covered_ms = h.volume_covered_ms + EXCLUDED.volume_covered_ms,
			retained_volume_byte_ms = h.retained_volume_byte_ms + EXCLUDED.retained_volume_byte_ms,
			egress_public_direct = h.egress_public_direct + EXCLUDED.egress_public_direct,
			egress_public_ingress = h.egress_public_ingress + EXCLUDED.egress_public_ingress,
			egress_private = h.egress_private + EXCLUDED.egress_private,
			egress_platform = h.egress_platform + EXCLUDED.egress_platform,
			egress_probe = h.egress_probe + EXCLUDED.egress_probe,
			first_start = LEAST(h.first_start, EXCLUDED.first_start),
			last_end = GREATEST(h.last_end, EXCLUDED.last_end)`,
		hours, kinds, ids, projects, environments, services,
		records, measured, gaps, uncertain, cpu, memory, memoryCovered,
		volume, volumeCovered, retained,
		egress[0], egress[1], egress[2], egress[3], egress[4], first, last)
	if err != nil {
		return fmt.Errorf("aggregate hourly usage: %w", err)
	}
	return nil
}

func (b *batch) writeProgress(ctx context.Context, tx *sql.Tx) error {
	if len(b.progress) == 0 {
		return nil
	}
	var kinds, ids, producers []string
	var ends []time.Time
	var sequences []int64
	for key, p := range b.progress {
		kinds, ids, producers = append(kinds, key.kind), append(ids, key.id), append(producers, p.producer)
		ends, sequences = append(ends, p.end), append(sequences, int64(p.sequence))
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_resource_progress AS p(resource_kind, resource_id, end_at, producer_id, sequence)
		SELECT * FROM unnest($1::TEXT[], $2::TEXT[], $3::TIMESTAMPTZ[], $4::TEXT[], $5::INT8[])
		ON CONFLICT (resource_kind, resource_id) DO UPDATE SET
			end_at = GREATEST(p.end_at, EXCLUDED.end_at), producer_id = EXCLUDED.producer_id, sequence = EXCLUDED.sequence`,
		kinds, ids, ends, producers, sequences)
	return err
}
