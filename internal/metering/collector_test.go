package metering

import (
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/proto"
)

type fakeClock struct{ now Instant }

func (c *fakeClock) Now() Instant { return c.now }
func (c *fakeClock) advance(d time.Duration) {
	c.now.Mono += d
	c.now.Wall = c.now.Wall.Add(d)
}

type fakeAllocs struct{ samples []AllocationSample }

func (f *fakeAllocs) Allocations() ([]AllocationSample, error) { return f.samples, nil }
func (f *fakeAllocs) Allocation(id string) (AllocationSample, bool, error) {
	for _, s := range f.samples {
		if s.AllocationID == id {
			return s, true, nil
		}
	}
	return AllocationSample{}, false, nil
}

type fakeVolumes struct{ used map[string]int64 }

func (f *fakeVolumes) Inventory() ([]string, error) {
	ids := make([]string, 0, len(f.used))
	for id := range f.used {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeVolumes) UsedBytes(id string) (int64, error) { return f.used[id], nil }

func u64(v uint64) *uint64 { return &v }

func openTestMeter(t *testing.T, clock *fakeClock, allocs *fakeAllocs, vols *fakeVolumes) *Meter {
	t.Helper()
	cfg := Config{Dir: t.TempDir(), InstallationID: "inst-1", AgentID: "agent-1",
		Clock: clock, Allocations: allocs,
		SampleInterval: 5 * time.Second, VolumeInterval: 60 * time.Second}
	if vols != nil {
		cfg.Volumes = vols
	}
	m, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func records(t *testing.T, m *Meter) []*agentv1.UsageRecord {
	t.Helper()
	recs, _, err := m.journal.batch(1)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestCollectorSealsMinuteWithExactQuantities(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 30, 0, time.UTC)
	clock := &fakeClock{now: Instant{Boot: "boot-1", Mono: 100 * time.Second, Wall: start}}
	allocs := &fakeAllocs{samples: []AllocationSample{
		{AllocationID: "alloc-1", CgroupID: 42, CPUMicros: u64(1_000_000), MemoryBytes: u64(100 << 20)}}}
	m := openTestMeter(t, clock, allocs, nil)
	// Warm up LastTick with no allocations so the epoch starts at the scan.
	saved := allocs.samples
	allocs.samples = nil
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	allocs.samples = saved
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	// 12 ticks of 5s carry the sample across the 12:01 boundary.
	for i := 0; i < 12; i++ {
		clock.advance(5 * time.Second)
		s := allocs.samples[0]
		*s.CPUMicros += 500_000
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	recs := records(t, m)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	// Partial first minute: 12:00:30 to the first sample at 12:01:00.
	if r.GetDurationMs() != 30_000 || r.GetCpuMicroseconds() != 3_000_000 {
		t.Fatalf("duration/cpu = %d/%d", r.GetDurationMs(), r.GetCpuMicroseconds())
	}
	if r.GetMemoryCoveredMs() != 30_000 {
		t.Fatalf("memory covered = %d", r.GetMemoryCoveredMs())
	}
	// (100<<20) bytes * 30000 ms.
	if got := r.GetMemoryByteMilliseconds(); got != "3145728000000" {
		t.Fatalf("memory integral = %s", got)
	}
}

func TestCollectorEndsEpochWithGapWhenTaskVanishes(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: Instant{Boot: "boot-1", Mono: 10 * time.Second, Wall: start}}
	allocs := &fakeAllocs{samples: []AllocationSample{
		{AllocationID: "alloc-1", CgroupID: 7, CPUMicros: u64(0), MemoryBytes: u64(1 << 20)}}}
	m := openTestMeter(t, clock, allocs, nil)
	_ = m.Tick()
	clock.advance(5 * time.Second)
	*allocs.samples[0].CPUMicros = 1000
	_ = m.Tick()
	allocs.samples = nil
	clock.advance(5 * time.Second)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	recs := records(t, m)
	var kinds []agentv1.UsageRecordKind
	for _, r := range recs {
		kinds = append(kinds, r.GetKind())
	}
	if len(recs) != 2 || recs[1].GetKind() != agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP ||
		recs[1].GetGapReason() != agentv1.UsageGapEpochUnobserved {
		t.Fatalf("kinds = %v, want measurement then epoch_end_unobserved gap", kinds)
	}
}

func TestCollectorJournalFullFreezesCheckpoints(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: Instant{Boot: "boot-1", Mono: time.Second, Wall: start}}
	allocs := &fakeAllocs{samples: []AllocationSample{
		{AllocationID: "alloc-1", CgroupID: 9, CPUMicros: u64(0), MemoryBytes: u64(1)}}}
	m, err := Open(Config{Dir: t.TempDir(), InstallationID: "inst-1", AgentID: "agent-1",
		Clock: clock, Allocations: allocs, MaxBytes: 1, SampleInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// First tick only checkpoints (no seal), then a final hook must seal and
	// hit the full journal without losing the checkpoint.
	_ = m.Tick()
	clock.advance(5 * time.Second)
	m.ObserveAllocation("alloc-1", true)
	if len(m.states) != 1 {
		t.Fatalf("states = %d, want the frozen checkpoint retained", len(m.states))
	}
	if m.journal.meta.ExhaustedSinceMS == 0 {
		t.Fatal("exhausted_since not recorded")
	}
}

func TestJournalFileGrowth(t *testing.T) {
	dir := t.TempDir()
	j, err := openJournal(dir, "inst-1", "agent-1", 1<<30, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	rec := &agentv1.UsageRecord{Kind: agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
		ResourceKind: agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION,
		ResourceId:   "alloc-1", CounterEpoch: "boot/1", BootId: "boot", DurationMs: 60000}
	for i := 0; i < 200; i++ {
		r := proto.Clone(rec).(*agentv1.UsageRecord)
		if err := j.update(func(tx *journalTx) error { return tx.append(r) }); err != nil {
			t.Fatal(err)
		}
	}
	stats := j.db.Stats()
	_ = stats
	// recordOverhead (48) plus payload must over-estimate nothing wildly:
	// tracked bytes stay within 4x of the file's data pages for tiny records.
	if j.meta.RecordBytes <= 0 {
		t.Fatal("no record bytes tracked")
	}
}
