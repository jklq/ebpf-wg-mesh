package logpipeline

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestSpool(t *testing.T, cfg SpoolConfig) *Spool {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	s, err := OpenSpool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func testRecord(key string, bytes int, at time.Time) Record {
	return Record{DropKey: DropKey{AllocationID: key, ServiceID: "svc", Stream: "stdout"}, ObservedAt: at, Payload: []byte(strings.Repeat("x", bytes))}
}
func mustAppend(t *testing.T, s *Spool, record Record) {
	t.Helper()
	if err := s.Append(record); err != nil {
		t.Fatal(err)
	}
}
func readSpool(t *testing.T, s *Spool, max int) ([]Record, Cursor) {
	t.Helper()
	rows, c, err := s.Read(max)
	if err != nil {
		t.Fatal(err)
	}
	return rows, c
}
func pendingDrops(t *testing.T, s *Spool) uint64 {
	t.Helper()
	rows, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, row := range rows {
		n += row.GetDroppedCount()
	}
	return n
}

func TestSpoolDurableCursorAndReplay(t *testing.T) {
	dir := t.TempDir()
	s := openTestSpool(t, SpoolConfig{Dir: dir, Retention: time.Hour})
	base := time.Now().UTC()
	for i := 0; i < 5; i++ {
		mustAppend(t, s, testRecord(fmt.Sprint(i), 1, base.Add(time.Duration(i)*time.Second)))
	}
	rows, c := readSpool(t, s, 2)
	if len(rows) != 2 || rows[1].DropKey.AllocationID != "1" {
		t.Fatalf("FIFO: %+v", rows)
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = readSpool(t, s, 2)
	if rows[0].DropKey.AllocationID != "2" {
		t.Fatal("commit skipped a record")
	}
	_ = s.Close() // outstanding read is deliberately not acknowledged
	s = openTestSpool(t, SpoolConfig{Dir: dir, Retention: time.Hour})
	rows, c = readSpool(t, s, 10)
	if len(rows) != 3 || rows[0].DropKey.AllocationID != "2" {
		t.Fatalf("unacknowledged replay: %+v", rows)
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RewindForReplay(base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, c = readSpool(t, s, 10)
	if len(rows) != 4 || rows[0].DropKey.AllocationID != "1" {
		t.Fatalf("retained replay: %+v", rows)
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolRewindNeverSkipsOldUnacknowledgedRecords(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	mustAppend(t, s, testRecord("old", 1, time.Now().Add(-24*time.Hour)))
	mustAppend(t, s, testRecord("new", 1, time.Now()))
	if err := s.RewindForReplay(time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 2 || rows[0].DropKey.AllocationID != "old" {
		t.Fatalf("old unaccepted record skipped: %+v", rows)
	}
}

func TestSpoolEvictionAndLossesCommitTogether(t *testing.T) {
	dir := t.TempDir()
	s := openTestSpool(t, SpoolConfig{Dir: dir, MaxBytes: 1500})
	for i := 0; i < 20; i++ {
		mustAppend(t, s, testRecord(fmt.Sprint(i%2), 100, time.Now()))
	}
	stats := spoolStats(t, s)
	if stats.Bytes > 1500 || stats.DroppedRecords == 0 {
		t.Fatalf("byte bound: %+v", stats)
	}
	if pendingDrops(t, s) != stats.DroppedRecords {
		t.Fatalf("missing loss accounting: %+v", stats)
	}
	rows, _ := readSpool(t, s, 100)
	if int64(len(rows))+int64(stats.DroppedRecords) != 20 {
		t.Fatalf("record conservation: %+v", stats)
	}
	s.Release()
	_ = s.Close()
	s = openTestSpool(t, SpoolConfig{Dir: dir, MaxBytes: 1500})
	if pendingDrops(t, s) != stats.DroppedRecords {
		t.Fatal("eviction losses did not survive restart")
	}
}

func TestSpoolRecordLimitAndExactAttribution(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 2})
	for _, key := range []string{"a", "b", "c", "d"} {
		mustAppend(t, s, testRecord(key, 1, time.Now()))
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 2 || rows[0].DropKey.AllocationID != "c" {
		t.Fatalf("record cap: %+v", rows)
	}
	drops, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, drop := range drops {
		got[drop.GetAllocationId()] += drop.GetDroppedCount()
	}
	if got["a"] != 1 || got["b"] != 1 || len(got) != 2 {
		t.Fatalf("attribution changed: %+v", drops)
	}
}

func TestSpoolPinsOnlyReadRecords(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 2})
	mustAppend(t, s, testRecord("flight", 1, time.Now()))
	mustAppend(t, s, testRecord("unread", 1, time.Now()))
	_, c := readSpool(t, s, 1)
	mustAppend(t, s, testRecord("new", 1, time.Now()))
	if err := s.RewindForReplay(time.Time{}); err == nil {
		t.Fatal("rewind accepted an outstanding batch")
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 1 || rows[0].DropKey.AllocationID != "new" {
		t.Fatalf("pinned record acknowledged incorrectly: %+v", rows)
	}
	drops, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].GetAllocationId() != "unread" {
		t.Fatalf("in-flight loss reported: %+v", drops)
	}
}

func TestSpoolRefusesAppendWhenAllCapacityIsPinned(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 1})
	mustAppend(t, s, testRecord("flight", 1, time.Now()))
	rows, c := readSpool(t, s, 1)
	if err := s.Append(testRecord("new", 1, time.Now())); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("expected full: %v", err)
	}
	if pendingDrops(t, s) != 0 {
		t.Fatal("failed admission reported accepted record as lost")
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DropKey.AllocationID != "flight" {
		t.Fatal("pinned record changed")
	}
	mustAppend(t, s, testRecord("new", 1, time.Now()))
}

func TestSpoolRejectOnFullRecoversAfterAcknowledgement(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 1, RejectOnFull: true})
	mustAppend(t, s, testRecord("a", 1, time.Now()))
	if err := s.Append(testRecord("b", 1, time.Now())); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("expected full: %v", err)
	}
	_, c := readSpool(t, s, 1)
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	if stats := spoolStats(t, s); stats.Bytes != 0 || stats.Records != 0 || stats.DroppedRecords != 0 {
		t.Fatalf("acknowledged capacity not reclaimed: %+v", stats)
	}
	mustAppend(t, s, testRecord("b", 1, time.Now()))
}

func TestSpoolRetentionExpiresAndYieldsToPressure(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(fmt.Sprint(pressure), func(t *testing.T) {
			cfg := SpoolConfig{Retention: time.Hour}
			if pressure {
				cfg.MaxRecords = 1
			}
			s := openTestSpool(t, cfg)
			at := time.Now()
			if !pressure {
				at = at.Add(-2 * time.Hour)
			}
			mustAppend(t, s, testRecord("ack", 1, at))
			_, c := readSpool(t, s, 1)
			if err := s.Commit(c, nil); err != nil {
				t.Fatal(err)
			}
			mustAppend(t, s, testRecord("new", 1, time.Now()))
			if stats := spoolStats(t, s); stats.Records != 1 || stats.DroppedRecords != 0 {
				t.Fatalf("replay copy did not yield: %+v", stats)
			}
		})
	}
}

func TestSpoolIdleAndStaleReadTokens(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	rows, old := readSpool(t, s, 1)
	if len(rows) != 0 {
		t.Fatal("new spool isn't empty")
	}
	s.Release()
	mustAppend(t, s, testRecord("a", 1, time.Now()))
	_, current := readSpool(t, s, 1)
	if err := s.Commit(old, nil); err == nil {
		t.Fatal("released token acknowledged a later read")
	}
	if err := s.Commit(current, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(current, nil); err == nil {
		t.Fatal("token acknowledged twice")
	}
}

func TestSpoolOversizedRecordDoesNotDisturbQueue(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxBytes: 1024})
	mustAppend(t, s, testRecord("a", 1, time.Now()))
	if err := s.Append(testRecord("too-big", 2048, time.Now())); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("size rejection: %v", err)
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 1 || pendingDrops(t, s) != 0 {
		t.Fatal("oversized append changed the queue")
	}
}

func TestSpoolDatabaseGrowthIsHardBoundedAndReusable(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxBytes: 32 << 10, MaxRecords: 10})
	for i := 0; i < 500; i++ {
		mustAppend(t, s, testRecord("a", 1024, time.Now()))
		_, c := readSpool(t, s, 10)
		if err := s.Commit(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	stats := spoolStats(t, s)
	if stats.DiskBytes > stats.DiskLimit || stats.Bytes != 0 {
		t.Fatalf("disk bound: %+v", stats)
	}
	// Force bbolt's disk cap without custom write hooks. The failed loss
	// transaction must preserve both the existing record and its cursor.
	mustAppend(t, s, testRecord("kept", 1, time.Now()))
	var additions []Drop
	for i := 0; i < 5000; i++ {
		additions = append(additions, Drop{Key: DropKey{AllocationID: fmt.Sprint(i) + strings.Repeat("z", 2048)}, Count: 1, Start: time.Now(), End: time.Now()})
	}
	if err := s.AddDrops(additions...); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("expected database disk cap: %v", err)
	}
	rows, c := readSpool(t, s, 10)
	if len(rows) != 1 || rows[0].DropKey.AllocationID != "kept" || pendingDrops(t, s) != 0 {
		t.Fatal("failed transaction changed accepted data")
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, testRecord("after", 1, time.Now()))
}

func TestSpoolRefusesRetiredFormatAndDamagedDatabase(t *testing.T) {
	for _, name := range []string{"seg-0000000000.log", "cursor.json", "drops.json", "pending-drops.json"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenSpool(SpoolConfig{Dir: dir}); err == nil {
			_ = s.Close()
			t.Fatalf("silently ignored %s", name)
		}
	}
	dir := t.TempDir()
	s := openTestSpool(t, SpoolConfig{Dir: dir})
	_ = s.Close()
	if err := os.WriteFile(filepath.Join(dir, SpoolFile), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenSpool(SpoolConfig{Dir: dir}); err == nil {
		_ = s.Close()
		t.Fatal("silently reset damaged database")
	}
}

func TestSpoolProcessCrashPreservesAcceptance(t *testing.T) {
	if dir := os.Getenv("LOGPIPELINE_CRASH_DIR"); dir != "" {
		s, err := OpenSpool(SpoolConfig{Dir: dir})
		if err != nil {
			os.Exit(2)
		}
		if err := s.Append(testRecord("durable", 1, time.Now())); err != nil {
			os.Exit(3)
		}
		if err := s.AddDrops(Drop{Key: DropKey{AllocationID: "loss"}, Count: 3, Start: time.Now(), End: time.Now()}); err != nil {
			os.Exit(4)
		}
		os.Exit(0) // no Close, defers or clean shutdown
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSpoolProcessCrashPreservesAcceptance$")
	cmd.Env = append(os.Environ(), "LOGPIPELINE_CRASH_DIR="+dir)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash writer: %v %s", err, raw)
	}
	s := openTestSpool(t, SpoolConfig{Dir: dir})
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 1 || rows[0].DropKey.AllocationID != "durable" || pendingDrops(t, s) != 3 {
		t.Fatal("durable acceptance lost after process exit")
	}
}

func spoolStats(t *testing.T, s *Spool) SpoolStats {
	t.Helper()
	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestSpoolBatchAdmissionIsAtomic(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 3, RejectOnFull: true})
	mustAppend(t, s, testRecord("existing", 1, time.Now()))
	err := s.Append(testRecord("first", 1, time.Now()), testRecord("second", 1, time.Now()), testRecord("third", 1, time.Now()))
	if !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("expected full: %v", err)
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 1 || rows[0].DropKey.AllocationID != "existing" {
		t.Fatalf("rejected batch left an accepted prefix: %+v", rows)
	}
}

func TestSpoolDiscardPreservesOtherRecordsAndExactGap(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	for _, owner := range []string{"good", "bad", "later"} {
		mustAppend(t, s, testRecord(owner, 1, time.Now()))
	}
	rows, c := readSpool(t, s, 10)
	if err := s.Discard(rows[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	drops, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].GetAllocationId() != "bad" || drops[0].GetReason() != ReasonCorruptSpool || drops[0].GetDroppedCount() != 1 {
		t.Fatalf("corruption attribution: %+v", drops)
	}
	if stats := spoolStats(t, s); stats.PendingRecords != 0 {
		t.Fatalf("discard skipped other acknowledgement: %+v", stats)
	}
	if err := s.Discard(rows[1]); err == nil {
		t.Fatal("stale record could be discarded")
	}
}

func TestSpoolMetadataChurnStaysWithinDiskBudget(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxBytes: 32 << 10})
	for round := 0; round < 30; round++ {
		var additions []Drop
		for i := 0; i < 200; i++ {
			additions = append(additions, Drop{Key: DropKey{AllocationID: fmt.Sprintf("%d-%d", round, i)}, Count: 1, Start: time.Now(), End: time.Now()})
		}
		if err := s.AddDrops(additions...); err != nil {
			t.Fatal(err)
		}
		drops, err := s.PendingDrops()
		if err != nil {
			t.Fatal(err)
		}
		if len(drops) != 200 {
			t.Fatalf("key churn did not release prior identities: %d", len(drops))
		}
		_, c := readSpool(t, s, 1)
		if err := s.Commit(c, drops); err != nil {
			t.Fatal(err)
		}
		if stats := spoolStats(t, s); stats.DiskBytes > stats.DiskLimit {
			t.Fatalf("metadata churn exceeded hard disk cap: %+v", stats)
		}
	}
}

func TestSpoolCollectionHandlesMixedTimesAndPressureInOneTransaction(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{MaxRecords: 5, Retention: 50 * time.Millisecond})
	now := time.Now()
	// Observation timestamps are deliberately not ordered by queue position.
	for i, at := range []time.Time{now, now.Add(time.Hour), now, now.Add(time.Hour), now.Add(time.Hour)} {
		mustAppend(t, s, testRecord(fmt.Sprint(i), 1, at))
	}
	_, c := readSpool(t, s, 10)
	if err := s.Commit(c, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	// The append first expires positions 0/2, then must collect the three
	// recent replay copies from the already modified bbolt node under pressure.
	if err := s.Append(testRecord("new-a", 1, time.Now()), testRecord("new-b", 1, time.Now()), testRecord("new-c", 1, time.Now())); err != nil {
		t.Fatal(err)
	}
	if stats := spoolStats(t, s); stats.Records != 3 || stats.PendingRecords != 3 || stats.DroppedRecords != 0 {
		t.Fatalf("collection skipped a replay copy or reported an accepted line as lost: %+v", stats)
	}
	rows, _ := readSpool(t, s, 10)
	if len(rows) != 3 || rows[0].DropKey.AllocationID != "new-a" {
		t.Fatalf("pressure changed pending records: %+v", rows)
	}
}
