//go:build integration

package controlplane

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/usage"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const usageTestInstallation = "installation-test"

type usageFixture struct {
	store        *persistence
	usage        *usage.Store
	environment  string
	project      string
	service      string
	allocation   string
	agent        string
	producerID   string
	nextSequence uint64
	clock        time.Time
}

func newUsageFixture(t *testing.T) *usageFixture {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	service, err := createService(ctx, store, "user-1", envID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{CpuMillis: 500, MemoryMebibytes: 128}), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if len(allocations) != 1 {
		t.Fatalf("allocations = %d, want 1", len(allocations))
	}
	// Fixture records start in the past; the attribution predates them.
	if _, err := store.db.Exec(`UPDATE usage_attributions SET created_at = created_at - interval '400 days'`); err != nil {
		t.Fatal(err)
	}
	return &usageFixture{
		store: store, usage: usage.New(store.db, usage.Config{}),
		environment: envID, project: service.ProjectID, service: service.ID,
		allocation: allocations[0].ID, agent: allocations[0].AgentID,
		producerID: uuid.NewString(), nextSequence: 1,
		clock: time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute),
	}
}

func (f *usageFixture) producer(t *testing.T) usage.Producer {
	t.Helper()
	var incarnation int64
	if err := f.store.db.QueryRow(`SELECT session_incarnation FROM agent_registrations WHERE id = $1`, f.agent).Scan(&incarnation); err != nil {
		t.Fatal(err)
	}
	return usage.Producer{InstallationID: usageTestInstallation, AgentID: f.agent, SessionIncarnation: uint64(incarnation)}
}

// minute returns the next one-minute allocation measurement.
func (f *usageFixture) minute(cpu uint64, memoryBytes int64, publicBytes uint64) *agentv1.UsageRecord {
	start := f.clock
	f.clock = f.clock.Add(time.Minute)
	record := &agentv1.UsageRecord{
		InstallationId: usageTestInstallation, ProducerId: f.producerID, Sequence: f.nextSequence,
		Kind:         agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
		ResourceKind: agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION, ResourceId: f.allocation,
		CounterEpoch: "boot-1/4242", BootId: "boot-1",
		Start: timestamppb.New(start), End: timestamppb.New(f.clock), DurationMs: 60_000,
		MeterVersion: agentv1.UsageMeterVersion, CpuMicroseconds: cpu,
		MemoryByteMilliseconds: agentv1.FormatUsageInteger(bigInt(memoryBytes * 60_000)), MemoryCoveredMs: 60_000,
		Egress: &agentv1.UsageEgress{PublicDirectBytes: publicBytes, PrivateBytes: 7},
	}
	f.nextSequence++
	return record
}

func bigInt(n int64) *big.Int { return big.NewInt(n) }

func (f *usageFixture) report(records ...*agentv1.UsageRecord) *agentv1.UsageReport {
	return &agentv1.UsageReport{
		AgentId: f.agent, InstallationId: usageTestInstallation, ProducerId: f.producerID, Records: records,
		SealedThrough: timestamppb.New(f.clock),
	}
}

func (f *usageFixture) ingest(t *testing.T, records ...*agentv1.UsageRecord) *agentv1.UsageAcknowledgement {
	t.Helper()
	ack, err := f.usage.Ingest(context.Background(), f.producer(t), f.report(records...))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return ack
}

type hourlyTotals struct {
	records                 int64
	cpu, memory, public     string
	measured, memoryCovered int64
}

func (f *usageFixture) hourly(t *testing.T) hourlyTotals {
	t.Helper()
	var h hourlyTotals
	if err := f.store.db.QueryRow(`SELECT COALESCE(sum(records), 0), COALESCE(sum(cpu_usec), 0)::TEXT, COALESCE(sum(memory_byte_ms), 0)::TEXT,
			COALESCE(sum(egress_public_direct), 0)::TEXT, COALESCE(sum(measured_ms), 0), COALESCE(sum(memory_covered_ms), 0)
		FROM usage_hourly WHERE resource_id = $1`, f.allocation).Scan(&h.records, &h.cpu, &h.memory, &h.public, &h.measured, &h.memoryCovered); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUsageIngestAdvancesCursorAndAggregatesOnce(t *testing.T) {
	f := newUsageFixture(t)
	var attributed string
	if err := f.store.db.QueryRow(`SELECT project_id FROM usage_attributions WHERE resource_kind = 'allocation' AND resource_id = $1 AND agent_id = $2`, f.allocation, f.agent).Scan(&attributed); err != nil {
		t.Fatalf("allocation attribution was not captured with the assignment: %v", err)
	}
	if attributed != f.project {
		t.Fatalf("attributed project = %s, want %s", attributed, f.project)
	}

	first, second := f.minute(1_000_000, 64<<20, 1000), f.minute(2_000_000, 64<<20, 500)
	ack := f.ingest(t, first, second)
	if ack.GetAcceptedThrough() != 2 || len(ack.GetRejections()) != 0 || ack.GetConflict() != "" {
		t.Fatalf("ack = %+v, want cursor 2 without rejections", ack)
	}
	want := hourlyTotals{records: 2, cpu: "3000000", memory: bigInt(2 * (64 << 20) * 60_000).String(), public: "1500", measured: 120_000, memoryCovered: 120_000}
	if got := f.hourly(t); got != want {
		t.Fatalf("hourly = %+v, want %+v", got, want)
	}

	// A retried report is a no-op, including after its raw rows are gone.
	ack = f.ingest(t, first, second)
	if ack.GetAcceptedThrough() != 2 {
		t.Fatalf("retry ack = %+v", ack)
	}
	if _, err := f.store.db.Exec(`DELETE FROM usage_intervals`); err != nil {
		t.Fatal(err)
	}
	ack = f.ingest(t, first, second)
	if ack.GetAcceptedThrough() != 2 || ack.GetConflict() != "" {
		t.Fatalf("replay after raw expiry ack = %+v", ack)
	}
	if got := f.hourly(t); got != want {
		t.Fatalf("replay changed aggregates: %+v, want %+v", got, want)
	}

	// A sequence gap stops acceptance at the contiguous cursor.
	f.nextSequence++
	skipped := f.minute(10, 1, 0)
	ack = f.ingest(t, skipped)
	if ack.GetAcceptedThrough() != 2 {
		t.Fatalf("out-of-order record advanced the cursor: %+v", ack)
	}
}

func TestUsageConflictingReplayBlocksProducer(t *testing.T) {
	f := newUsageFixture(t)
	record := f.minute(1_000_000, 1<<20, 0)
	f.ingest(t, record)
	changed := f.minute(0, 0, 0)
	changed.Sequence, changed.Start, changed.End = record.Sequence, record.Start, record.End
	changed.CpuMicroseconds = 9_000_000
	ack := f.ingest(t, changed)
	if ack.GetConflict() == "" || ack.GetAcceptedThrough() != 1 {
		t.Fatalf("conflicting replay ack = %+v, want conflict at cursor 1", ack)
	}
	// The producer stays blocked; later records are not accepted.
	f.nextSequence = 2
	ack = f.ingest(t, f.minute(1, 1, 0))
	if ack.GetConflict() == "" || ack.GetAcceptedThrough() != 1 {
		t.Fatalf("blocked producer ack = %+v", ack)
	}
	if got := f.hourly(t); got.cpu != "1000000" {
		t.Fatalf("conflict changed aggregates: %+v", got)
	}
}

func TestUsageIngestFencesStaleSessions(t *testing.T) {
	f := newUsageFixture(t)
	stale := f.producer(t)
	if _, err := upsertTestAgent(t, f.store, context.Background(), agentHello(f.agent)); err != nil {
		t.Fatal(err)
	}
	_, err := f.usage.Ingest(context.Background(), stale, f.report(f.minute(1, 1, 0)))
	if !errors.Is(err, usage.ErrStaleSession) {
		t.Fatalf("stale session ingest error = %v, want ErrStaleSession", err)
	}
	if ack := f.ingest(t, f.minuteAt(1)); ack.GetAcceptedThrough() != 1 {
		t.Fatalf("current session ack = %+v", ack)
	}
}

// minuteAt rebuilds a measurement for an explicit sequence.
func (f *usageFixture) minuteAt(sequence uint64) *agentv1.UsageRecord {
	f.nextSequence = sequence
	return f.minute(1, 1, 0)
}

func TestUsageRejectsUnauthorizedAndOverlappingRecords(t *testing.T) {
	f := newUsageFixture(t)
	foreign := f.minute(1, 1, 0)
	foreign.ResourceId = uuid.NewString()
	valid := f.minute(5, 1, 0)
	overlapping := f.minute(1, 1, 0)
	overlapping.Start = timestamppb.New(valid.GetStart().AsTime().Add(30 * time.Second))
	overlapping.DurationMs = overlapping.GetEnd().AsTime().Sub(overlapping.GetStart().AsTime()).Milliseconds()
	implausible := f.minute(1<<40, 1, 0)
	ack := f.ingest(t, foreign, valid, overlapping, implausible)
	if ack.GetAcceptedThrough() != 4 {
		t.Fatalf("ack = %+v, want every record stored", ack)
	}
	reasons := map[uint64]string{}
	for _, rejection := range ack.GetRejections() {
		reasons[rejection.GetSequence()] = rejection.GetReason()
	}
	if reasons[1] != usage.RejectUnknownAttribution || reasons[2] != "" || reasons[3] != usage.RejectOverlap || reasons[4] == "" {
		t.Fatalf("rejections = %v", reasons)
	}
	var stored int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM usage_intervals WHERE status = 'rejected' AND payload IS NOT NULL`).Scan(&stored); err != nil || stored != 3 {
		t.Fatalf("rejected rows with payload = %d (%v), want 3", stored, err)
	}
	if got := f.hourly(t); got.cpu != "5" || got.records != 1 {
		t.Fatalf("only the valid record may aggregate: %+v", got)
	}
}

func TestUsageReplacementJournalSupersedesOldProducer(t *testing.T) {
	f := newUsageFixture(t)
	f.ingest(t, f.minute(1, 1, 0))
	old := f.producerID
	oldRecord := f.minute(1, 1, 0)

	f.producerID, f.nextSequence = uuid.NewString(), 1
	f.clock = f.clock.Add(5 * time.Minute)
	if ack := f.ingest(t, f.minute(3, 1, 0)); ack.GetAcceptedThrough() != 1 {
		t.Fatalf("replacement ack = %+v", ack)
	}
	var state string
	if err := f.store.db.QueryRow(`SELECT state FROM usage_producers WHERE producer_id = $1`, old).Scan(&state); err != nil || state != "superseded" {
		t.Fatalf("old producer state = %q (%v)", state, err)
	}
	var gaps int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM usage_producer_gaps WHERE producer_id = $1 AND reason = 'journal_replaced'`, old).Scan(&gaps); err != nil || gaps != 1 {
		t.Fatalf("journal replacement gaps = %d (%v)", gaps, err)
	}
	report := f.report(oldRecord)
	report.ProducerId = old
	if _, err := f.usage.Ingest(context.Background(), f.producer(t), report); !errors.Is(err, usage.ErrProducerSuperseded) {
		t.Fatalf("old journal ingest error = %v, want ErrProducerSuperseded", err)
	}
}

func TestUsageLostRecordsBecomeProducerGap(t *testing.T) {
	f := newUsageFixture(t)
	f.ingest(t, f.minute(1, 1, 0))
	f.nextSequence = 6
	report := f.report(f.minute(2, 1, 0))
	report.LostThrough = 5
	ack, err := f.usage.Ingest(context.Background(), f.producer(t), report)
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetAcceptedThrough() != 6 {
		t.Fatalf("ack = %+v, want cursor past the lost range", ack)
	}
	var first, last int64
	if err := f.store.db.QueryRow(`SELECT first_sequence, last_sequence FROM usage_producer_gaps WHERE reason = 'acknowledged_records_lost'`).Scan(&first, &last); err != nil || first != 2 || last != 5 {
		t.Fatalf("lost range = %d..%d (%v), want 2..5", first, last, err)
	}
}

// Usage survives catalog collection: a delayed record for a deleted service
// is still accepted under its retained attribution.
func TestUsageAcceptsDelayedRecordsAfterCatalogDeletion(t *testing.T) {
	f := newUsageFixture(t)
	ctx := context.Background()
	f.ingest(t, f.minute(1, 1, 0))
	if err := deleteService(ctx, f.store, "user-1", f.service); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE services SET delete_expires_at = now() - interval '1 second' WHERE id = $1`, f.service); err != nil {
		t.Fatal(err)
	}
	if collected, err := f.store.hardDeleteExpired(ctx, expiredDeletion{Kind: expiredDeletionService, ID: f.service}, time.Now()); err != nil || !collected {
		t.Fatalf("collect service: %v %v", collected, err)
	}
	if ack := f.ingest(t, f.minute(2, 1, 0)); ack.GetAcceptedThrough() != 2 || len(ack.GetRejections()) != 0 {
		t.Fatalf("delayed record ack = %+v", ack)
	}
	var project string
	if err := f.store.db.QueryRow(`SELECT project_id FROM usage_hourly WHERE resource_id = $1 LIMIT 1`, f.allocation).Scan(&project); err != nil || project != f.project {
		t.Fatalf("aggregate project = %q (%v)", project, err)
	}
}

func TestUsageHourlyRebuildMatchesIncrementalAggregation(t *testing.T) {
	f := newUsageFixture(t)
	f.clock = time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour).Add(-3 * time.Minute)
	var records []*agentv1.UsageRecord
	for i := range 8 {
		records = append(records, f.minute(uint64(1000*(i+1)), int64(i+1)<<20, uint64(i)))
	}
	f.ingest(t, records...)
	before := f.hourly(t)
	if err := f.usage.RebuildHourly(context.Background(), f.clock.Add(-3*time.Hour), f.clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if after := f.hourly(t); after != before {
		t.Fatalf("rebuilt hourly = %+v, incremental = %+v", after, before)
	}
}

func TestUsageRetentionKeepsReplayProtection(t *testing.T) {
	f := newUsageFixture(t)
	f.clock = time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Minute)
	old := f.minute(5, 1, 0)
	f.ingest(t, old)
	if _, err := f.store.db.Exec(`CREATE TABLE usage_intervals_20200101 PARTITION OF usage_intervals FOR VALUES FROM ('2020-01-01T00:00:00Z') TO ('2020-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	result, err := f.usage.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.DroppedPartitions != 1 {
		t.Fatalf("dropped partitions = %d, want the expired daily partition", result.DroppedPartitions)
	}
	var tomorrow bool
	if err := f.store.db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "usage_intervals_"+time.Now().UTC().AddDate(0, 0, 1).Format("20060102")).Scan(&tomorrow); err != nil || !tomorrow {
		t.Fatalf("maintenance did not prepare tomorrow's partition: %v", err)
	}
	var raw int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM usage_intervals`).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw rows after retention = %d (%v); result %+v", raw, err, result)
	}
	if ack := f.ingest(t, old); ack.GetAcceptedThrough() != 1 || ack.GetConflict() != "" {
		t.Fatalf("replay after retention ack = %+v", ack)
	}
	if got := f.hourly(t); got.records != 1 {
		t.Fatalf("replay after retention re-aggregated: %+v", got)
	}
}

func (f *usageFixture) query(t *testing.T, q usage.Query) *platformv1.ProjectUsage {
	t.Helper()
	project, err := f.store.authorizer().AuthorizeProject(context.Background(), testUser("user-1"), f.project, authz.Read)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.usage.ProjectUsage(context.Background(), usage.ProjectAccess(project), q)
	if err != nil {
		t.Fatalf("ProjectUsage: %v", err)
	}
	return result
}

func TestUsageQueryBucketsByStartWithExactTotals(t *testing.T) {
	f := newUsageFixture(t)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	f.clock = hour.Add(10 * time.Minute)
	// One vCPU and 1 GiB for three minutes, then a record that starts in
	// this hour and ends in the next.
	records := []*agentv1.UsageRecord{f.minute(60_000_000, 1<<30, 100), f.minute(60_000_000, 1<<30, 100), f.minute(60_000_000, 1<<30, 100)}
	f.clock = hour.Add(59*time.Minute + 30*time.Second)
	records = append(records, f.minute(60_000_000, 1<<30, 100))
	f.ingest(t, records...)

	minutes := f.query(t, usage.Query{Start: hour.Add(10 * time.Minute), End: hour.Add(13 * time.Minute), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE})
	if len(minutes.GetBuckets()) != 3 {
		t.Fatalf("minute buckets = %d, want 3", len(minutes.GetBuckets()))
	}
	for _, bucket := range minutes.GetBuckets() {
		usage := bucket.GetUsage()
		if usage.GetCpuMicroseconds() != "60000000" || usage.GetAverageVcpu() != 1 || usage.GetAverageMemoryBytes() != 1<<30 || usage.GetEstimated() {
			t.Fatalf("minute bucket %s = %+v, want one vCPU and 1 GiB", bucket.GetStart().AsTime(), usage)
		}
	}

	hours := f.query(t, usage.Query{Start: hour, End: hour.Add(2 * time.Hour), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_HOUR, ResourceBuckets: true})
	if got := hours.GetBuckets()[0].GetUsage().GetCpuMicroseconds(); got != "240000000" {
		t.Fatalf("hour bucket cpu = %s; the straddling record must count in the hour it starts", got)
	}
	if got := hours.GetBuckets()[1].GetUsage().GetCpuMicroseconds(); got != "0" {
		t.Fatalf("next hour cpu = %s, want 0", got)
	}
	total := hours.GetTotal()
	if total.GetCpuMicroseconds() != "240000000" || total.GetPublicEgressBytes() != "400" || total.GetPrivateBytes() != "28" || total.GetMeasuredMilliseconds() != 240_000 {
		t.Fatalf("total = %+v", total)
	}
	// The window ends at the watermark, 30 seconds into the second hour.
	if want := 240e6 / 1000 / 3630e3; total.GetAverageVcpu() != want || hours.GetBuckets()[1].GetComplete() {
		t.Fatalf("average vCPU = %v, want %v over the window up to the watermark", total.GetAverageVcpu(), want)
	}
	if len(hours.GetResources()) != 1 || hours.GetResources()[0].GetName() != "web" || len(hours.GetResources()[0].GetBuckets()) != 2 {
		t.Fatalf("resources = %+v", hours.GetResources())
	}
	if resource := hours.GetResources()[0].GetUsage(); resource.GetAverageVcpu() != 1 {
		t.Fatalf("resource average vCPU = %v, want 1 while running", resource.GetAverageVcpu())
	}

	days := f.query(t, usage.Query{Start: hour, End: hour.Add(time.Minute), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_DAY})
	if len(days.GetBuckets()) != 1 || !days.GetBuckets()[0].GetStart().AsTime().Equal(hour.Truncate(24*time.Hour)) || days.GetTotal().GetCpuMicroseconds() != "240000000" {
		t.Fatalf("day query = %+v", days.GetBuckets())
	}
	if _, err := f.usage.ProjectUsage(context.Background(), usage.ProjectAccess(authzProject(t, f)), usage.Query{Start: hour, End: hour.Add(48 * time.Hour), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE}); !errors.Is(err, usage.ErrInvalidQuery) {
		t.Fatalf("oversized minute range error = %v", err)
	}
}

func authzProject(t *testing.T, f *usageFixture) authz.Project {
	t.Helper()
	project, err := f.store.authorizer().AuthorizeProject(context.Background(), testUser("user-1"), f.project, authz.Read)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestUsageQueryReportsCoverageGapsAndFreshness(t *testing.T) {
	f := newUsageFixture(t)
	start := f.clock
	partial := f.minute(1_000_000, 1<<30, 0)
	partial.MemoryByteMilliseconds = agentv1.FormatUsageInteger(bigInt(int64(1<<30) * 30_000))
	partial.MemoryCoveredMs = 30_000
	partial.Quality = uint32(agentv1.UsageQuality_USAGE_QUALITY_GAUGE_GAP)
	gap := f.minute(0, 0, 0)
	gap.Kind, gap.GapReason, gap.MemoryByteMilliseconds, gap.MemoryCoveredMs, gap.Egress = agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP, agentv1.UsageGapEpochUnobserved, "", 0, nil
	f.ingest(t, partial, gap)

	result := f.query(t, usage.Query{Start: start, End: start.Add(2 * time.Minute), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE})
	first := result.GetBuckets()[0].GetUsage()
	if first.GetAverageMemoryBytes() != 1<<30 || !first.GetEstimated() || first.GetMemoryCoveredMilliseconds() != 30_000 {
		t.Fatalf("partially covered bucket = %+v, want the covered average scaled to measured time", first)
	}
	if second := result.GetBuckets()[1].GetUsage(); second.GetGapMilliseconds() != 60_000 || second.GetCpuMicroseconds() != "0" {
		t.Fatalf("gap bucket = %+v", second)
	}
	if len(result.GetGaps()) != 1 || result.GetGaps()[0].GetReason() != agentv1.UsageGapEpochUnobserved {
		t.Fatalf("gaps = %+v", result.GetGaps())
	}
	// Sealed through 28 minutes ago: the project's current agent is stale.
	if !result.GetStale() || result.GetWatermark().AsTime().After(time.Now().Add(-time.Minute)) {
		t.Fatalf("stale = %v watermark = %v", result.GetStale(), result.GetWatermark().AsTime())
	}
	report := f.report()
	report.SealedThrough = timestamppb.Now()
	if _, err := f.usage.Ingest(context.Background(), f.producer(t), report); err != nil {
		t.Fatal(err)
	}
	if fresh := f.query(t, usage.Query{Start: start, End: start.Add(time.Minute), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE}); fresh.GetStale() {
		t.Fatalf("fresh producer still stale: %s", fresh.GetStaleReason())
	}
}

func TestUsageVolumeRetainedAfterDeletion(t *testing.T) {
	f := newUsageFixture(t)
	ctx := context.Background()
	volume, err := f.store.catalog.createScheduledVolume(ctx, testUser("user-1"), f.environment, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	db, err := createService(ctx, f.store, "user-1", f.environment, "db", volumeServiceSpec("data", "/data"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE usage_attributions SET created_at = created_at - interval '400 days' WHERE resource_id = $1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	volumeMinute := func(start time.Time) *agentv1.UsageRecord {
		record := f.minute(0, 0, 0)
		record.ResourceKind, record.ResourceId, record.CounterEpoch = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME, volume.ID, "boot-1"
		record.Start, record.End = timestamppb.New(start), timestamppb.New(start.Add(time.Minute))
		record.MemoryByteMilliseconds, record.MemoryCoveredMs, record.Egress = "", 0, nil
		record.VolumeByteMilliseconds, record.VolumeCoveredMs = agentv1.FormatUsageInteger(bigInt(int64(10<<20)*60_000)), 60_000
		return record
	}
	before := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	f.ingest(t, volumeMinute(before))
	if err := deleteService(ctx, f.store, "user-1", db.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE services SET delete_expires_at = now() - interval '1 second' WHERE id = $1`, db.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.hardDeleteExpired(ctx, expiredDeletion{Kind: expiredDeletionService, ID: db.ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.catalog.deleteVolume(ctx, testUser("user-1"), volume.ID, "data"); err != nil {
		t.Fatal(err)
	}
	f.ingest(t, volumeMinute(time.Now().UTC().Add(time.Minute).Truncate(time.Minute)))

	result := f.query(t, usage.Query{Start: before, End: time.Now().Add(3 * time.Minute), Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_HOUR, VolumeID: volume.ID})
	total := result.GetTotal()
	perMinute := bigInt(int64(10<<20) * 60_000)
	if total.GetVolumeByteMilliseconds() != new(big.Int).Mul(perMinute, big.NewInt(2)).String() || total.GetRetainedVolumeByteMilliseconds() != perMinute.String() {
		t.Fatalf("volume usage = %s retained = %s", total.GetVolumeByteMilliseconds(), total.GetRetainedVolumeByteMilliseconds())
	}
	if len(result.GetResources()) != 1 || result.GetResources()[0].GetDeletedAt() == nil || result.GetResources()[0].GetName() != "data" {
		t.Fatalf("volume resource = %+v", result.GetResources())
	}
}

func TestUsageQueryAuthorization(t *testing.T) {
	f := newUsageFixture(t)
	ctx := context.Background()
	f.ingest(t, f.minute(42, 1, 0))
	service := newPlatformService(f.store.platform(), noopNotifier{}, noopIngress{}, testDelivery(f.store), withUsage(f.usage, f.store.authorizer()))
	request := &platformv1.GetProjectUsageRequest{
		ProjectId: f.project, StartTime: timestamppb.New(f.clock.Add(-time.Hour)), EndTime: timestamppb.New(f.clock.Add(time.Hour)),
		Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_HOUR,
	}
	read := func(user string) (*platformv1.ProjectUsage, error) {
		return service.GetProjectUsage(contextWithDelegatedUser(user, ""), request)
	}
	grantWorkspaceRole(t, f.store, ctx, f.project, "viewer", "viewer")
	if result, err := read("viewer"); err != nil || result.GetTotal().GetCpuMicroseconds() != "42" {
		t.Fatalf("viewer read = %v, %v", result.GetTotal(), err)
	}
	if _, err := read("outsider"); status.Code(err) != codes.NotFound {
		t.Fatalf("outsider read = %v, want NotFound", err)
	}

	// Deleted resources stay visible inside the surviving project.
	if err := deleteService(ctx, f.store, "user-1", f.service); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE services SET delete_expires_at = now() - interval '1 second' WHERE id = $1`, f.service); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.hardDeleteExpired(ctx, expiredDeletion{Kind: expiredDeletionService, ID: f.service}, time.Now()); err != nil {
		t.Fatal(err)
	}
	result, err := read("viewer")
	if err != nil || len(result.GetResources()) != 1 || result.GetResources()[0].GetDeletedAt() == nil || result.GetResources()[0].GetName() != "web" {
		t.Fatalf("deleted service history = %+v, %v", result.GetResources(), err)
	}

	// Retained attribution grants nothing: once the project is collected,
	// members lose access and only operators can read its history.
	if _, err := f.store.catalog.deleteProject(ctx, testUser("user-1"), f.project, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE projects SET delete_expires_at = now() - interval '1 second' WHERE id = $1`, f.project); err != nil {
		t.Fatal(err)
	}
	if collected, err := f.store.hardDeleteExpired(ctx, expiredDeletion{Kind: expiredDeletionProject, ID: f.project}, time.Now()); err != nil || !collected {
		t.Fatalf("collect project: %v %v", collected, err)
	}
	if _, err := read("user-1"); status.Code(err) != codes.NotFound {
		t.Fatalf("former member read = %v, want NotFound", err)
	}
	if _, err := f.store.db.Exec(`INSERT INTO platform_operators(user_id, created_at) VALUES ('ops', now())`); err != nil {
		t.Fatal(err)
	}
	if result, err := read("ops"); err != nil || result.GetTotal().GetCpuMicroseconds() != "42" {
		t.Fatalf("operator read of deleted project = %v, %v", result.GetTotal(), err)
	}
}
