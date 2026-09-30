package logpipeline

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

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
	// Cap reached: a new key must recycle the LRU bucket, not be rejected forever.
	if !l.Allow("c") {
		t.Fatal("new key permanently rejected past the key cap")
	}
	if l.Keys() > 2 {
		t.Fatalf("key table must stay bounded, got %d", l.Keys())
	}
	now = now.Add(time.Second)
	// The evicted key returns with a fresh bucket.
	if !l.Allow("a") {
		t.Fatal("recycled key permanently rejected")
	}
}

func TestLimiterRecyclesEmptyKey(t *testing.T) {
	l := newLimiter(1, 1, 1, time.Now)
	if !l.Allow("") || !l.Allow("next") {
		t.Fatal("a full table with an empty key must accept the next key")
	}
	if l.Keys() != 1 {
		t.Fatalf("tracked %d keys, want 1", l.Keys())
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

// A cap below the default segment size still bounds the spool:
// the segment clamps to the cap.
func TestTruncateLineKeepsValidUTF8(t *testing.T) {
	t.Parallel()
	// A mid-rune cut would be invalid UTF-8, which protobuf rejects — silently dropping the line.
	line := strings.Repeat("é", MaxLogLineBytes)
	got, truncated := TruncateLine(line)
	if !truncated {
		t.Fatal("oversized line must truncate")
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated line must stay valid UTF-8")
	}
	if len(got) > MaxLogLineBytes {
		t.Fatalf("truncated line grew past the cap: %d", len(got))
	}
}
