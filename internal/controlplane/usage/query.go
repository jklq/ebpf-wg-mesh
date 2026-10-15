package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrInvalidQuery is a usage query outside the documented limits.
var ErrInvalidQuery = errors.New("invalid usage query")

// Access proves that the caller may read one project's usage. Retained
// attribution never grants access: a project member reads through the
// project's current workspace membership, and an operator may read the
// history of a project whose catalog row no longer exists.
type Access struct{ projectID string }

// ProjectAccess grants a project reader its project's usage, including
// deleted resources that belonged to it.
func ProjectAccess(project authz.Project) Access { return Access{projectID: project.ID()} }

// DeletedProjectAccess grants an operator the history of a fully deleted
// project. The caller must verify that the project row is gone.
func DeletedProjectAccess(_ authz.Operator, projectID string) Access {
	return Access{projectID: projectID}
}

// Query selects a project's usage. Start and End are widened to whole
// buckets of Resolution.
type Query struct {
	Start, End      time.Time
	Resolution      platformv1.UsageResolution
	EnvironmentID   string
	ServiceID       string
	AllocationID    string
	VolumeID        string
	ResourceBuckets bool
}

type resolution struct {
	step     time.Duration
	maxRange time.Duration
	hourly   bool
}

var resolutions = map[platformv1.UsageResolution]resolution{
	platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE: {step: time.Minute, maxRange: 24 * time.Hour},
	platformv1.UsageResolution_USAGE_RESOLUTION_HOUR:   {step: time.Hour, maxRange: 31 * 24 * time.Hour, hourly: true},
	platformv1.UsageResolution_USAGE_RESOLUTION_DAY:    {step: 24 * time.Hour, maxRange: 366 * 24 * time.Hour, hourly: true},
}

// maxCells bounds bucket rows read for one query.
const maxCells = 250_000

// maxGaps bounds listed gap intervals.
const maxGaps = 200

// sums are exact accumulated quantities for one set of intervals.
type sums struct {
	records, measured, gap, uncertain int64
	memoryCovered, volumeCovered      int64
	cpu, memory, volume, retained     big.Int
	egress                            [5]big.Int
	// adjustedMemory and adjustedVolume scale each resource's covered
	// integral to its measured time.
	adjustedMemory, adjustedVolume big.Rat
	first, last                    time.Time
}

func (s *sums) add(o *sums) {
	s.records += o.records
	s.measured += o.measured
	s.gap += o.gap
	s.uncertain += o.uncertain
	s.memoryCovered += o.memoryCovered
	s.volumeCovered += o.volumeCovered
	s.cpu.Add(&s.cpu, &o.cpu)
	s.memory.Add(&s.memory, &o.memory)
	s.volume.Add(&s.volume, &o.volume)
	s.retained.Add(&s.retained, &o.retained)
	for i := range s.egress {
		s.egress[i].Add(&s.egress[i], &o.egress[i])
	}
	s.adjustedMemory.Add(&s.adjustedMemory, &o.adjustedMemory)
	s.adjustedVolume.Add(&s.adjustedVolume, &o.adjustedVolume)
	if !o.first.IsZero() && (s.first.IsZero() || o.first.Before(s.first)) {
		s.first = o.first
	}
	if o.last.After(s.last) {
		s.last = o.last
	}
}

// adjust derives coverage-scaled gauge integrals for one resource's cell.
func (s *sums) adjust() {
	scale := func(integral *big.Int, covered int64, out *big.Rat) {
		if covered <= 0 || integral.Sign() == 0 {
			return
		}
		out.SetFrac(new(big.Int).Mul(integral, big.NewInt(s.measured)), big.NewInt(covered))
	}
	scale(&s.memory, s.memoryCovered, &s.adjustedMemory)
	scale(&s.volume, s.volumeCovered, &s.adjustedVolume)
}

func (s *sums) quantities(window int64) *platformv1.UsageQuantities {
	public := new(big.Int).Add(&s.egress[0], &s.egress[1])
	q := &platformv1.UsageQuantities{
		CpuMicroseconds:                s.cpu.String(),
		MemoryByteMilliseconds:         s.memory.String(),
		VolumeByteMilliseconds:         s.volume.String(),
		RetainedVolumeByteMilliseconds: s.retained.String(),
		PublicEgressBytes:              public.String(),
		PublicDirectBytes:              s.egress[0].String(),
		PublicIngressBytes:             s.egress[1].String(),
		PrivateBytes:                   s.egress[2].String(),
		PlatformBytes:                  s.egress[3].String(),
		ProbeBytes:                     s.egress[4].String(),
		MeasuredMilliseconds:           s.measured,
		GapMilliseconds:                s.gap,
		MemoryCoveredMilliseconds:      s.memoryCovered,
		VolumeCoveredMilliseconds:      s.volumeCovered,
		UncertainMilliseconds:          s.uncertain,
		WindowMilliseconds:             max(window, 0),
		Estimated:                      s.uncertain > 0 || s.adjustedMemory.Cmp(new(big.Rat).SetInt(&s.memory)) != 0 || s.adjustedVolume.Cmp(new(big.Rat).SetInt(&s.volume)) != 0,
	}
	if window > 0 {
		w := big.NewInt(window)
		q.AverageVcpu, _ = new(big.Rat).SetFrac(&s.cpu, new(big.Int).Mul(w, big.NewInt(1000))).Float64()
		q.AverageMemoryBytes, _ = new(big.Rat).Quo(&s.adjustedMemory, new(big.Rat).SetInt(w)).Float64()
		q.AverageVolumeBytes, _ = new(big.Rat).Quo(&s.adjustedVolume, new(big.Rat).SetInt(w)).Float64()
	}
	return q
}

type cellKey struct {
	bucket time.Time
	resourceKey
}

// ProjectUsage reads one project's usage. An interval belongs to the bucket
// that contains its start, so a bucket can include a few seconds of usage
// past its nominal end. Bucket and total averages divide by the bucket time
// up to the measurement watermark; resource averages divide by the
// resource's own observed time.
func (s *Store) ProjectUsage(ctx context.Context, access Access, q Query) (*platformv1.ProjectUsage, error) {
	res, ok := resolutions[q.Resolution]
	if !ok {
		return nil, fmt.Errorf("%w: resolution is required", ErrInvalidQuery)
	}
	start, end := alignDown(q.Start.UTC(), res.step), alignUp(q.End.UTC(), res.step)
	if !end.After(start) || end.Sub(start) > res.maxRange {
		return nil, fmt.Errorf("%w: range must be positive and at most %s for this resolution", ErrInvalidQuery, res.maxRange)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	now = now.UTC()

	filter, args := q.filter(access.projectID)
	cells, err := readCells(ctx, tx, res, start, end, filter, args)
	if err != nil {
		return nil, err
	}
	watermark, staleAgents, err := s.watermark(ctx, tx, filter, args)
	if err != nil {
		return nil, err
	}
	horizon := now
	if !watermark.IsZero() && watermark.Before(now) {
		horizon = watermark
	}

	out := &platformv1.ProjectUsage{
		ProjectId: access.projectID, StartTime: timestamppb.New(start), EndTime: timestamppb.New(end),
		Resolution: q.Resolution, MeterVersion: agentv1.UsageMeterVersion,
	}
	if !watermark.IsZero() {
		out.Watermark = timestamppb.New(watermark)
	}
	if staleAgents > 0 {
		out.Stale = true
		out.StaleReason = fmt.Sprintf("usage from %d fleet agent(s) running this project's resources is more than %s behind", staleAgents, s.cfg.StaleAfter)
	}
	retention := s.cfg.HourlyRetention
	if !res.hourly {
		retention = s.cfg.RawRetention
	}
	out.RetainedSince = timestamppb.New(now.Add(-retention))

	window := func(from, to time.Time) int64 {
		if to.After(horizon) {
			to = horizon
		}
		return to.Sub(from).Milliseconds()
	}
	var total sums
	buckets := make(map[time.Time]*sums)
	resources := make(map[resourceKey]*sums)
	resourceBuckets := make(map[resourceKey]map[time.Time]*sums)
	for key, cell := range cells {
		total.add(cell)
		addTo(buckets, key.bucket, cell)
		r := resources[key.resourceKey]
		if r == nil {
			r = &sums{}
			resources[key.resourceKey] = r
		}
		r.add(cell)
		if q.ResourceBuckets {
			if resourceBuckets[key.resourceKey] == nil {
				resourceBuckets[key.resourceKey] = make(map[time.Time]*sums)
			}
			addTo(resourceBuckets[key.resourceKey], key.bucket, cell)
		}
	}
	out.Total = total.quantities(window(start, end))
	series := func(values map[time.Time]*sums) []*platformv1.UsageBucket {
		var list []*platformv1.UsageBucket
		for bucket := start; bucket.Before(end); bucket = nextBucket(bucket, res.step) {
			next := nextBucket(bucket, res.step)
			cell := values[bucket]
			if cell == nil {
				cell = &sums{}
			}
			list = append(list, &platformv1.UsageBucket{
				Start: timestamppb.New(bucket), End: timestamppb.New(next),
				Usage: cell.quantities(window(bucket, next)), Complete: !watermark.IsZero() && !next.After(watermark),
			})
		}
		return list
	}
	out.Buckets = series(buckets)
	meta, err := resourceMetadata(ctx, tx, access.projectID, resources)
	if err != nil {
		return nil, err
	}
	for key, r := range resources {
		resource := meta[key]
		if resource == nil {
			resource = &platformv1.UsageResource{Kind: key.kind, ResourceId: key.id}
		}
		resource.Usage = r.quantities(r.measured + r.gap)
		if !r.first.IsZero() {
			resource.FirstStart, resource.LastEnd = timestamppb.New(r.first), timestamppb.New(r.last)
		}
		if q.ResourceBuckets {
			resource.Buckets = series(resourceBuckets[key])
		}
		out.Resources = append(out.Resources, resource)
	}
	sort.Slice(out.Resources, func(i, j int) bool {
		a, b := out.Resources[i], out.Resources[j]
		if a.GetKind() != b.GetKind() {
			return a.GetKind() < b.GetKind()
		}
		if a.GetName() != b.GetName() {
			return a.GetName() < b.GetName()
		}
		return a.GetResourceId() < b.GetResourceId()
	})
	out.Gaps, out.GapsTruncated, err = readGaps(ctx, tx, start, end, filter, args)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func addTo(values map[time.Time]*sums, bucket time.Time, cell *sums) {
	current := values[bucket]
	if current == nil {
		current = &sums{}
		values[bucket] = current
	}
	current.add(cell)
}

func alignDown(t time.Time, step time.Duration) time.Time { return t.Truncate(step) }

func alignUp(t time.Time, step time.Duration) time.Time {
	down := t.Truncate(step)
	if down.Equal(t) {
		return t
	}
	return down.Add(step)
}

func nextBucket(t time.Time, step time.Duration) time.Time { return t.Add(step) }

// filter renders the project scope and optional resource filters. The
// project predicate always applies, so a filter can only narrow.
func (q Query) filter(projectID string) (string, []any) {
	clauses := []string{"project_id = $1"}
	args := []any{projectID}
	add := func(column, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	add("environment_id", q.EnvironmentID)
	add("service_id", q.ServiceID)
	if q.AllocationID != "" {
		args = append(args, q.AllocationID)
		clauses = append(clauses, fmt.Sprintf("(resource_kind = 'allocation' AND resource_id = $%d)", len(args)))
	}
	if q.VolumeID != "" {
		args = append(args, q.VolumeID)
		clauses = append(clauses, fmt.Sprintf("(resource_kind = 'volume' AND resource_id = $%d)", len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

func readCells(ctx context.Context, tx *sql.Tx, res resolution, start, end time.Time, filter string, args []any) (map[cellKey]*sums, error) {
	n := len(args)
	args = append(args, start, end)
	var query string
	if res.hourly {
		bucket := "hour"
		if res.step == 24*time.Hour {
			bucket = "date_trunc('day', hour, 'UTC')"
		}
		query = `SELECT ` + bucket + `, resource_kind, resource_id, sum(records), sum(measured_ms), sum(gap_ms), sum(uncertain_ms),
				sum(memory_covered_ms), sum(volume_covered_ms), sum(cpu_usec)::TEXT, sum(memory_byte_ms)::TEXT, sum(volume_byte_ms)::TEXT,
				sum(retained_volume_byte_ms)::TEXT, sum(egress_public_direct)::TEXT, sum(egress_public_ingress)::TEXT,
				sum(egress_private)::TEXT, sum(egress_platform)::TEXT, sum(egress_probe)::TEXT, min(first_start), max(last_end)
			  FROM usage_hourly
			 WHERE ` + filter + fmt.Sprintf(` AND hour >= $%d AND hour < $%d`, n+1, n+2) + `
			 GROUP BY 1, 2, 3`
	} else {
		query = `SELECT to_timestamp(floor(extract(epoch FROM start_at) / 60) * 60), resource_kind, resource_id, count(*),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'measurement'), 0),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'gap'), 0),
				COALESCE(sum(duration_ms) FILTER (WHERE record_kind = 'measurement' AND quality & 1 <> 0), 0),
				sum(memory_covered_ms), sum(volume_covered_ms), sum(cpu_usec)::TEXT, sum(memory_byte_ms)::TEXT, sum(volume_byte_ms)::TEXT,
				COALESCE(sum(volume_byte_ms) FILTER (WHERE retained), 0)::TEXT, sum(egress_public_direct)::TEXT, sum(egress_public_ingress)::TEXT,
				sum(egress_private)::TEXT, sum(egress_platform)::TEXT, sum(egress_probe)::TEXT, min(start_at), max(end_at)
			  FROM usage_intervals
			 WHERE status = 'accepted' AND ` + filter + fmt.Sprintf(` AND start_at >= $%d AND start_at < $%d`, n+1, n+2) + `
			 GROUP BY 1, 2, 3`
	}
	rows, err := tx.QueryContext(ctx, query+fmt.Sprintf(" LIMIT %d", maxCells+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cells := make(map[cellKey]*sums)
	for rows.Next() {
		var key cellKey
		cell := &sums{}
		var cpu, memory, volume, retained string
		var egress [5]string
		if err := rows.Scan(&key.bucket, &key.kind, &key.id, &cell.records, &cell.measured, &cell.gap, &cell.uncertain,
			&cell.memoryCovered, &cell.volumeCovered, &cpu, &memory, &volume, &retained,
			&egress[0], &egress[1], &egress[2], &egress[3], &egress[4], &cell.first, &cell.last); err != nil {
			return nil, err
		}
		for _, field := range []struct {
			text string
			out  *big.Int
		}{{cpu, &cell.cpu}, {memory, &cell.memory}, {volume, &cell.volume}, {retained, &cell.retained},
			{egress[0], &cell.egress[0]}, {egress[1], &cell.egress[1]}, {egress[2], &cell.egress[2]}, {egress[3], &cell.egress[3]}, {egress[4], &cell.egress[4]}} {
			if _, ok := field.out.SetString(field.text, 10); !ok {
				return nil, fmt.Errorf("decode usage quantity %q", field.text)
			}
		}
		key.bucket = key.bucket.UTC()
		cell.adjust()
		cells[key] = cell
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cells) > maxCells {
		return nil, fmt.Errorf("%w: the range has more than %d resource buckets; narrow the range or filter", ErrInvalidQuery, maxCells)
	}
	return cells, nil
}

// watermark is the oldest sealed-through time among the producers of agents
// that run the project's current resources. It also counts agents whose
// producer is missing or older than StaleAfter.
func (s *Store) watermark(ctx context.Context, tx *sql.Tx, filter string, args []any) (time.Time, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.sealed_through, statement_timestamp()
		  FROM (SELECT DISTINCT agent_id FROM usage_attributions WHERE ended_at IS NULL AND `+filter+`) a
		  LEFT JOIN usage_producers p ON p.agent_id = a.agent_id AND p.state <> 'superseded'`, args...)
	if err != nil {
		return time.Time{}, 0, err
	}
	defer rows.Close()
	var watermark time.Time
	stale := 0
	for rows.Next() {
		var sealed sql.NullTime
		var now time.Time
		if err := rows.Scan(&sealed, &now); err != nil {
			return time.Time{}, 0, err
		}
		if !sealed.Valid {
			stale++
			continue
		}
		if sealed.Time.Before(now.Add(-s.cfg.StaleAfter)) {
			stale++
		}
		if watermark.IsZero() || sealed.Time.Before(watermark) {
			watermark = sealed.Time.UTC()
		}
	}
	return watermark, stale, rows.Err()
}

func readGaps(ctx context.Context, tx *sql.Tx, start, end time.Time, filter string, args []any) ([]*platformv1.UsageGap, bool, error) {
	n := len(args)
	args = append(args, start, end)
	rows, err := tx.QueryContext(ctx, `SELECT resource_kind, resource_id, start_at, end_at, gap_reason FROM usage_intervals
		 WHERE status = 'accepted' AND record_kind = 'gap' AND `+filter+fmt.Sprintf(` AND start_at >= $%d AND start_at < $%d`, n+1, n+2)+`
		UNION ALL
		SELECT 'agent', '', g.start_at, g.end_at, g.reason FROM usage_producer_gaps g
		 WHERE g.end_at > $`+fmt.Sprint(n+1)+` AND g.start_at < $`+fmt.Sprint(n+2)+` AND g.agent_id IN (
			SELECT agent_id FROM usage_attributions WHERE `+filter+fmt.Sprintf(` AND created_at < $%d AND (ended_at IS NULL OR ended_at > $%d)`, n+2, n+1)+`)
		 ORDER BY 3, 1, 2 LIMIT `+fmt.Sprint(maxGaps+1), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var gaps []*platformv1.UsageGap
	for rows.Next() {
		var gap platformv1.UsageGap
		var from, to time.Time
		if err := rows.Scan(&gap.ResourceKind, &gap.ResourceId, &from, &to, &gap.Reason); err != nil {
			return nil, false, err
		}
		gap.Start, gap.End = timestamppb.New(from), timestamppb.New(to)
		gaps = append(gaps, &gap)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(gaps) > maxGaps {
		return gaps[:maxGaps], true, nil
	}
	return gaps, false, nil
}

// resourceMetadata names resources from retained attribution, preferring
// current catalog names while the rows still exist.
func resourceMetadata(ctx context.Context, tx *sql.Tx, projectID string, resources map[resourceKey]*sums) (map[resourceKey]*platformv1.UsageResource, error) {
	out := make(map[resourceKey]*platformv1.UsageResource, len(resources))
	if len(resources) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(resources))
	for key := range resources {
		ids = append(ids, key.id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.resource_kind, a.resource_id, a.environment_id, COALESCE(e.name, a.environment_name),
			a.service_id, COALESCE(s.name, v.name, a.resource_name), a.deployment_id,
			CASE
				WHEN a.resource_kind = 'allocation' AND s.id IS NULL THEN COALESCE(a.ended_at, a.created_at)
				WHEN a.resource_kind = 'volume' AND v.id IS NULL THEN COALESCE(a.deleted_at, a.ended_at, a.created_at)
				ELSE LEAST(s.deleted_at, v.deleted_at, e.deleted_at, p.deleted_at)
			END
		  FROM usage_attributions a
		  LEFT JOIN environments e ON e.id = a.environment_id
		  LEFT JOIN projects p ON p.id = a.project_id
		  LEFT JOIN services s ON a.resource_kind = 'allocation' AND s.id = a.service_id
		  LEFT JOIN volumes v ON a.resource_kind = 'volume' AND v.id = a.resource_id
		 WHERE a.project_id = $1 AND a.resource_id = ANY($2)`, projectID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key resourceKey
		r := &platformv1.UsageResource{}
		var deleted sql.NullTime
		if err := rows.Scan(&key.kind, &key.id, &r.EnvironmentId, &r.EnvironmentName, &r.ServiceId, &r.Name, &r.DeploymentId, &deleted); err != nil {
			return nil, err
		}
		r.Kind, r.ResourceId = key.kind, key.id
		if deleted.Valid {
			r.DeletedAt = timestamppb.New(deleted.Time)
		}
		out[key] = r
	}
	return out, rows.Err()
}

// ProjectExists reports whether a project's catalog row still exists.
func (s *Store) ProjectExists(ctx context.Context, projectID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)`, projectID).Scan(&exists)
	return exists, err
}
