package logpipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestSpool(t *testing.T, cfg SpoolConfig) *Spool {
	t.Helper()
	cfg.Dir = t.TempDir()
	cfg.SyncWrites = false
	s, err := OpenSpool(cfg)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSpoolAppendReadCommit(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i, key := range []string{"a", "b", "a"} {
		if err := s.Append(key, "id", now, []byte{byte(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 3 || recs[0].Key != "a" || recs[1].Key != "b" || recs[2].Key != "a" {
		t.Fatalf("unexpected records: %+v", recs)
	}
	if !recs[0].ObservedAt.Equal(now) {
		t.Fatalf("timestamp not preserved: %v", recs[0].ObservedAt)
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	recs, _, err = s.Read(10)
	if err != nil {
		t.Fatalf("Read after commit: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected drained spool, got %d records", len(recs))
	}
	if stats := s.Stats(); stats.DroppedRecords != 0 || stats.CorruptRecords != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestSpoolSurvivesRestartWithUncommittedReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := OpenSpool(SpoolConfig{Dir: dir, SyncWrites: true})
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := s.Append("alloc", "id", now, []byte{byte(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(2)
	if err != nil || len(recs) != 2 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenSpool(SpoolConfig{Dir: dir, SyncWrites: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	recs, _, err = reopened.Read(10)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if len(recs) != 3 || recs[0].Payload[0] != 2 {
		t.Fatalf("expected replay of 3 uncommitted records, got %d", len(recs))
	}
}

func TestSpoolEvictsOldestPastByteCapWithPerKeyDrops(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxBytes: 900, MaxSegmentBytes: 300})
	now := time.Now().UTC()
	payload := make([]byte, 100)
	for i := 0; i < 20; i++ {
		key := "hot"
		if i%5 == 0 {
			key = "cold"
		}
		if err := s.Append(key, "id", now, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	stats := s.Stats()
	if stats.DroppedRecords == 0 {
		t.Fatal("expected drops past the byte cap")
	}
	evicted, corrupt := s.DrainDrops()
	if corrupt != nil {
		t.Fatalf("evictions must not count as corruption: %v", corrupt)
	}
	if len(evicted) == 0 {
		t.Fatal("expected per-key drop accounting")
	}
	var total uint64
	for _, count := range evicted {
		total += count
	}
	if total != stats.DroppedRecords {
		t.Fatalf("drained %d drops but stats report %d", total, stats.DroppedRecords)
	}
	if evicted, _ := s.DrainDrops(); evicted != nil {
		t.Fatal("drain did not clear drop accounting")
	}
	// Unshipped survivors are still readable.
	recs, _, err := s.Read(100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("expected surviving records after eviction")
	}
}

func TestSpoolCommittedSegmentsVanishSilently(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxBytes: 600, MaxSegmentBytes: 200})
	now := time.Now().UTC()
	payload := make([]byte, 80)
	for i := 0; i < 4; i++ {
		if err := s.Append("a", "id", now, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(100)
	if err != nil || len(recs) != 4 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	before := s.Stats()
	for i := 0; i < 20; i++ {
		if err := s.Append("a", "id", now, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if i%2 == 1 {
			recs, cursor, err := s.Read(100)
			if err != nil || len(recs) == 0 {
				t.Fatalf("Read: %v %d", err, len(recs))
			}
			if err := s.Commit(cursor); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		}
	}
	if stats := s.Stats(); stats.DroppedRecords != before.DroppedRecords {
		t.Fatalf("shipped evictions must not count as drops: %+v", stats)
	}
}

func TestSpoolTruncatesTornTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := OpenSpool(SpoolConfig{Dir: dir})
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	now := time.Now().UTC()
	if err := s.Append("a", "id", now, []byte("whole")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Simulate a crash mid-write: append a partial frame.
	seg := filepath.Join(dir, "seg-0000000000.log")
	f, err := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	full := encodeRecord("a", "id", now, []byte("torn"))
	if _, err := f.Write(full[:len(full)-3]); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	_ = f.Close()
	reopened, err := OpenSpool(SpoolConfig{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	recs, _, err := reopened.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 1 || string(recs[0].Payload) != "whole" {
		t.Fatalf("torn tail corrupted recovery: %+v", recs)
	}
	if stats := reopened.Stats(); stats.CorruptRecords == 0 {
		t.Fatal("expected the torn tail to count as corrupt")
	}
	// The damaged frame still reveals its key, so the loss is
	// attributable as a corrupt_spool drop.
	evicted, corrupt := reopened.DrainDrops()
	if evicted != nil {
		t.Fatalf("torn tail must not count as eviction: %v", evicted)
	}
	if corrupt["a"] != 1 {
		t.Fatalf("torn tail loss not attributed to its key: %v", corrupt)
	}
}

func TestSpoolCompactsMidFileCorruptionWithKeyAttribution(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	r1 := encodeRecord("k1", "id-1", now, []byte("one"))
	r2 := encodeRecord("k2", "id-2", now, []byte("two"))
	r3 := encodeRecord("k3", "id-3", now, []byte("three"))
	r2[len(r2)-1] ^= 0xff // Flip a checksum byte mid-file.
	segment := append(append(append([]byte(nil), r1...), r2...), r3...)
	if err := os.WriteFile(filepath.Join(dir, "seg-0000000000.log"), segment, 0o600); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	s, err := OpenSpool(SpoolConfig{Dir: dir})
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer s.Close()
	recs, _, err := s.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 2 || recs[0].Key != "k1" || recs[1].Key != "k3" {
		t.Fatalf("corrupt frame broke recovery: %+v", recs)
	}
	if stats := s.Stats(); stats.CorruptRecords != 1 || stats.DroppedRecords != 1 {
		t.Fatalf("corrupt frame not counted once: %+v", stats)
	}
	evicted, corrupt := s.DrainDrops()
	if evicted != nil {
		t.Fatalf("corruption must not count as eviction: %v", evicted)
	}
	if corrupt["k2"] != 1 {
		t.Fatalf("corrupt frame loss not attributed to its key: %v", corrupt)
	}
}

func TestSpoolRewindForReplayReplaysRecentWindow(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{})
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := s.Append("a", "id", ts, []byte{byte(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(100)
	if err != nil || len(recs) != 6 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.RewindForReplay(base.Add(4 * time.Second)); err != nil {
		t.Fatalf("RewindForReplay: %v", err)
	}
	recs, _, err = s.Read(100)
	if err != nil {
		t.Fatalf("Read after rewind: %v", err)
	}
	if len(recs) != 2 || recs[0].Payload[0] != 4 {
		t.Fatalf("expected replay of last 2 records, got %d", len(recs))
	}
}

func TestSpoolRewindForReplayNeverSkipsUncommitted(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := s.Append("a", "id", ts, []byte{byte(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// Ship and commit only the first two records; the rest are old
	// but were never accepted by the backend.
	recs, cursor, err := s.Read(2)
	if err != nil || len(recs) != 2 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The window starts past every unshipped record: the cursor
	// must not advance over them.
	if err := s.RewindForReplay(base.Add(10 * time.Second)); err != nil {
		t.Fatalf("RewindForReplay: %v", err)
	}
	recs, _, err = s.Read(100)
	if err != nil {
		t.Fatalf("Read after rewind: %v", err)
	}
	if len(recs) != 4 || recs[0].Payload[0] != 2 {
		t.Fatalf("rewind skipped unshipped records: got %d", len(recs))
	}
	// The window starting inside the unshipped range must not skip
	// the older unshipped records either.
	if err := s.RewindForReplay(base.Add(4 * time.Second)); err != nil {
		t.Fatalf("RewindForReplay: %v", err)
	}
	recs, _, err = s.Read(100)
	if err != nil {
		t.Fatalf("Read after second rewind: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("rewind moved forward over unshipped records: got %d", len(recs))
	}
}

func TestLimiterBurstsThenRefills(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newLimiter(10, 3, 16, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("burst line %d denied", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("expected denial past burst")
	}
	if got := l.DroppedSince("a"); got != 1 {
		t.Fatalf("dropped = %d", got)
	}
	now = now.Add(time.Second)
	allowed := 0
	for i := 0; i < 11; i++ {
		if l.Allow("a") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("refill allowed %d, want burst-capped 3", allowed)
	}
	drops := l.DrainDrops()
	if drops["a"] != 1+8 {
		t.Fatalf("drained drops = %v", drops)
	}
	// Other keys are independent.
	if !l.Allow("b") {
		t.Fatal("independent key denied")
	}
}

func TestLimiterUnlimitedWhenRateIsZero(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !l.Allow("a") {
			t.Fatal("zero rate must allow everything")
		}
	}
	if l.DrainDrops() != nil {
		t.Fatal("unlimited limiter must not count drops")
	}
}

func TestLimiterRecyclesKeysPastTheCap(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newLimiter(1, 1, 2, func() time.Time { return now })

	if !l.Allow("a") {
		t.Fatal("first key denied")
	}
	now = now.Add(time.Second)
	if !l.Allow("b") {
		t.Fatal("second key denied")
	}
	now = now.Add(time.Second)
	// Cap reached: a new key must recycle the least recently used
	// bucket instead of being rejected for the rest of the process.
	if !l.Allow("c") {
		t.Fatal("new key permanently rejected past the key cap")
	}
	if l.Keys() > 2 {
		t.Fatalf("key table must stay bounded, got %d", l.Keys())
	}
	now = now.Add(time.Second)
	// The evicted key returns with a fresh bucket instead of being
	// permanently rejected.
	if !l.Allow("a") {
		t.Fatal("recycled key permanently rejected")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 9, 1, 12, 30, 0, 123, time.UTC)
	token := EncodeCursor(ts, "ag:a:b:c:1")
	gotTS, gotID, err := DecodeCursor(token)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !gotTS.Equal(ts) || gotID != "ag:a:b:c:1" {
		t.Fatalf("round trip failed: %v %q", gotTS, gotID)
	}
	if _, _, err := DecodeCursor("!!!"); err == nil {
		t.Fatal("expected invalid token error")
	}
	if ts, id, err := DecodeCursor(""); err != nil || !ts.IsZero() || id != "" {
		t.Fatalf("empty token must decode to start: %v %q %v", ts, id, err)
	}
}

func TestTruncateLine(t *testing.T) {
	t.Parallel()
	short := "hello"
	if got, truncated := TruncateLine(short); got != short || truncated {
		t.Fatalf("short line mangled: %q %v", got, truncated)
	}
	long := make([]byte, MaxLogLineBytes+10)
	for i := range long {
		long[i] = 'x'
	}
	got, truncated := TruncateLine(string(long))
	if !truncated || len(got) != MaxLogLineBytes {
		t.Fatalf("long line: len=%d truncated=%v", len(got), truncated)
	}
}

func TestNormalizeAttributes(t *testing.T) {
	t.Parallel()
	if NormalizeAttributes(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	if NormalizeAttributes(map[string]string{}) != nil {
		t.Fatal("empty must become nil")
	}
	many := map[string]string{}
	for i := 0; i < 100; i++ {
		many[string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}
	if got := NormalizeAttributes(many); len(got) != MaxAttributesPerLine {
		t.Fatalf("attributes not capped: %d", len(got))
	}
	if got := NormalizeAttributes(map[string]string{"": "v"}); got != nil {
		t.Fatal("empty key must drop")
	}
	if NormalizeEvent("allocation.crash_loop") == "" || NormalizeEvent("has space") != "" {
		t.Fatal("event normalization wrong")
	}
	if NormalizeDropReason("nope") != ReasonIngestOverflow {
		t.Fatal("unknown reason must map to ingest_overflow")
	}
}

func TestStableEventID(t *testing.T) {
	t.Parallel()
	a := StableEventID("allocation.crash_loop", "agent-1", "alloc-1", "7", "2026-09-22T01:02:03Z")
	b := StableEventID("allocation.crash_loop", "agent-1", "alloc-1", "7", "2026-09-22T01:02:03Z")
	if a != b {
		t.Fatalf("identical facts produced different IDs: %q vs %q", a, b)
	}
	if len(a) < 4 || a[:3] != "sy:" {
		t.Fatalf("event ID %q must keep the sy: prefix", a)
	}
	if StableEventID("allocation.crash_loop", "agent-1", "alloc-2", "7", "2026-09-22T01:02:03Z") == a {
		t.Fatal("different allocation must produce a different ID")
	}
	if StableEventID("allocation.crash_loop", "agent-1", "alloc-1", "7", "2026-09-22T03:00:00Z") == a {
		t.Fatal("different observation window must produce a different ID")
	}
}

// A cap below the default segment size must still bound the spool:
// the segment size clamps to the cap so rotation and eviction keep
// one active file from blowing past MaxBytes.
func TestSpoolSmallCapBoundsActiveSegment(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxBytes: 1000})
	payload := make([]byte, 100)
	for i := 0; i < 20; i++ {
		if err := s.Append("hot", fmt.Sprintf("id-%02d", i), time.Now().UTC(), payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	stats := s.Stats()
	if stats.Bytes > 1000 {
		t.Fatalf("spool exceeded its cap: %+v", stats)
	}
	if stats.DroppedRecords == 0 {
		t.Fatal("cap shedding must count as drops")
	}
}

// Committed sealed segments survive for the retention horizon as the
// replay copy: agents are acknowledged at queue admission, so a
// backend crash before durable ingest must leave a reconnect
// something to re-send (server-side dedup absorbs the overlap).
func TestSpoolRetainsCommittedSegmentsForReplay(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxSegmentBytes: 200, Retention: time.Hour})
	now := time.Now().UTC()
	payload := make([]byte, 80)
	for i := 0; i < 20; i++ {
		if err := s.Append("alloc-1", fmt.Sprintf("id-%02d", i), now, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(100)
	if err != nil || len(recs) != 20 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if stats := s.Stats(); stats.Segments < 2 {
		t.Fatalf("expected sealed segments to retain, got %+v", stats)
	}
	// The reconnect replay window re-sends the acknowledged records.
	if err := s.RewindForReplay(now.Add(-time.Minute)); err != nil {
		t.Fatalf("RewindForReplay: %v", err)
	}
	recs, _, err = s.Read(100)
	if err != nil || len(recs) != 20 {
		t.Fatalf("committed records not replayable: %v %d", err, len(recs))
	}
}

// Past the retention horizon, committed sealed segments collect so
// the replay copy cannot grow without bound.
func TestSpoolCollectsCommittedSegmentsAfterRetention(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxSegmentBytes: 200, Retention: 50 * time.Millisecond})
	payload := make([]byte, 80)
	for i := 0; i < 20; i++ {
		if err := s.Append("alloc-1", fmt.Sprintf("id-%02d", i), time.Now().UTC(), payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, cursor, err := s.Read(100)
	if err != nil || len(recs) != 20 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	// The next commit collects the sealed segments once their newest
	// record is older than the horizon.
	if err := s.Append("alloc-1", "id-late", time.Now().UTC(), payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	recs, cursor, err = s.Read(100)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Read: %v %d", err, len(recs))
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if stats := s.Stats(); stats.Segments != 1 {
		t.Fatalf("committed segments not collected past retention: %+v", stats)
	}
}

func TestSpoolDoesNotEvictInFlightBatch(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{MaxBytes: 900, MaxSegmentBytes: 300})
	now := time.Now().UTC()
	small := make([]byte, 8)
	payload := make([]byte, 100)
	// "in-flight" is the read batch; "kept" is unread but shares the
	// pinned segment.
	if err := s.Append("flight", "in-flight", now, small); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append("kept", "kept", now, small); err != nil {
		t.Fatalf("Append: %v", err)
	}
	records, cursor, err := s.Read(1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 || records[0].ID != "in-flight" {
		t.Fatalf("unexpected batch: %+v", records)
	}
	// Overflow the spool while the batch is in flight: eviction must
	// skip the pinned segment even though older data normally goes
	// first — the batch may already be delivered, and evicting the
	// segment would report a false gap and skip the unread record.
	for i := 0; i < 20; i++ {
		if err := s.Append("hot", fmt.Sprintf("id-%02d", i), now, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	evicted, _ := s.DrainDrops()
	if evicted["flight"] != 0 || evicted["kept"] != 0 {
		t.Fatalf("pinned segment must not count as evicted drops: %v", evicted)
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit under eviction pressure: %v", err)
	}
	rest, _, err := s.Read(1000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var sawKept, sawCommitted bool
	for _, rec := range rest {
		switch rec.ID {
		case "kept":
			sawKept = true
		case "in-flight":
			sawCommitted = true
		}
	}
	if !sawKept {
		t.Fatal("unread record in the pinned segment was skipped without a gap")
	}
	if sawCommitted {
		t.Fatal("committed record must not be read again")
	}
}

func TestSpoolAppendRollsBackPartialFrames(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{})
	now := time.Now().UTC()
	if err := s.Append("a", "id-a", now, []byte("whole")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Simulate a failed append that left a partial frame in the
	// active segment and repair it exactly as Append does.
	full := encodeRecord("b", "id-b", now, []byte("partial"))
	if _, err := s.active.Write(full[:len(full)-3]); err != nil {
		t.Fatalf("write partial frame: %v", err)
	}
	s.mu.Lock()
	s.discardPartialAppendLocked()
	s.mu.Unlock()
	if err := s.Append("c", "id-c", now, []byte("after")); err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	recs, cursor, err := s.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 2 || recs[0].Key != "a" || recs[1].Key != "c" {
		t.Fatalf("partial frame hid later records: %+v", recs)
	}
	if err := s.Commit(cursor); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	reopened, err := OpenSpool(SpoolConfig{Dir: s.dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rest, _, err := reopened.Read(10)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("committed records replayed: %+v", rest)
	}
	if stats := reopened.Stats(); stats.CorruptRecords != 0 {
		t.Fatalf("rolled-back frame resurfaced as corruption: %+v", stats)
	}
}

func TestSpoolAppendFailureSealsUnrepairableSegment(t *testing.T) {
	t.Parallel()
	s := openTestSpool(t, SpoolConfig{})
	now := time.Now().UTC()
	if err := s.Append("a", "id-a", now, []byte("whole")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Make the active file unrepairable: appends fail and the partial
	// frame cannot be rolled back, so the segment must be sealed and
	// later records must land in a fresh one.
	_ = s.active.Close()
	if err := s.Append("b", "id-b", now, []byte("lost")); err == nil {
		t.Fatal("append to a broken segment must fail")
	}
	if err := s.Append("c", "id-c", now, []byte("after")); err != nil {
		t.Fatalf("append after sealing: %v", err)
	}
	recs, _, err := s.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 2 || recs[0].Key != "a" || recs[1].Key != "c" {
		t.Fatalf("sealed segment hid later records: %+v", recs)
	}
}

func TestSpoolCompactionKeepsUnshippedRecordsAfterCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	r1 := encodeRecord("k1", "id-1", now, []byte("shipped"))
	r2 := encodeRecord("k2", "id-2", now, []byte("two"))
	r3 := encodeRecord("k3", "id-3", now, []byte("unshipped-after"))
	r2[len(r2)-1] ^= 0xff // Flip a checksum byte before the cursor.
	segment := append(append(append([]byte(nil), r1...), r2...), r3...)
	if err := os.WriteFile(filepath.Join(dir, "seg-0000000000.log"), segment, 0o600); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	// The durable cursor claims the corrupt record shipped; the
	// record after it did not and must survive compaction.
	cursor := fmt.Sprintf(`{"segment":0,"offset":%d}`, len(r1)+len(r2))
	if err := os.WriteFile(filepath.Join(dir, "cursor.json"), []byte(cursor), 0o600); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	s, err := OpenSpool(SpoolConfig{Dir: dir})
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer s.Close()
	recs, _, err := s.Read(10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 1 || recs[0].Key != "k3" {
		t.Fatalf("compaction let a stale cursor skip unshipped records: %+v", recs)
	}
	evicted, corrupt := s.DrainDrops()
	if evicted != nil || corrupt["k2"] != 1 {
		t.Fatalf("corrupt frame accounting: evicted=%v corrupt=%v", evicted, corrupt)
	}
}
