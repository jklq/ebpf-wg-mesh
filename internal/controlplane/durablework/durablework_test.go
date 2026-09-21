package durablework

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetryDelayStaysWithinJitterBand(t *testing.T) {
	t.Parallel()
	for attempt := int64(1); attempt <= 3; attempt++ {
		for i := 0; i < 200; i++ {
			got := RetryDelay(attempt, 10*time.Second, time.Hour)
			unjittered := 10 * time.Second << (attempt - 1)
			low, high := unjittered-(unjittered/5), unjittered+(unjittered/5)
			if got < low || got > high {
				t.Fatalf("attempt %d delay %v outside [%v, %v]", attempt, got, low, high)
			}
		}
	}
}

func TestRetryDelayCapsAtMax(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		if got := RetryDelay(100, time.Second, 30*time.Second); got < 24*time.Second || got > 36*time.Second {
			t.Fatalf("capped delay %v outside jittered max band", got)
		}
	}
}

func TestRetryDelayDefaults(t *testing.T) {
	t.Parallel()
	if got := RetryDelay(0, 0, 0); got < 4*time.Second || got > 6*time.Second {
		t.Fatalf("default first delay %v, want jittered %v", got, DefaultBaseDelay)
	}
	if got := RetryDelay(-5, 0, 0); got < 4*time.Second || got > 6*time.Second {
		t.Fatalf("negative attempt delay %v, want jittered %v", got, DefaultBaseDelay)
	}
}

func TestSanitizeError(t *testing.T) {
	t.Parallel()
	if got := SanitizeError(nil); got != "" {
		t.Fatalf("nil error sanitized to %q", got)
	}
	if got := SanitizeError(errors.New("  boom  ")); got != "boom" {
		t.Fatalf("sanitized to %q", got)
	}
	nul := SanitizeError(errors.New("a\x00b"))
	if strings.ContainsRune(nul, '\x00') {
		t.Fatalf("NUL survived sanitization: %q", nul)
	}
	long := SanitizeError(errors.New(strings.Repeat("x", MaxLastErrorRunes+500)))
	if got := len([]rune(long)); got != MaxLastErrorRunes {
		t.Fatalf("sanitized length %d runes, want %d", got, MaxLastErrorRunes)
	}
	multibyte := SanitizeError(errors.New(strings.Repeat("é", MaxLastErrorRunes+10)))
	if got := len([]rune(multibyte)); got != MaxLastErrorRunes {
		t.Fatalf("multibyte sanitized length %d runes, want %d", got, MaxLastErrorRunes)
	}
}

func TestRecordTerminal(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]bool{
		StatePending: false, StateLeased: false,
		StateSucceeded: true, StateFailed: true, StateDead: true,
		"bogus": false,
	} {
		if got := (Record{State: state}).Terminal(); got != want {
			t.Fatalf("state %q terminal = %v, want %v", state, got, want)
		}
	}
}

func TestLeaseHeldBy(t *testing.T) {
	t.Parallel()
	rec := Record{State: StateLeased, OwnerID: "a", OwnerEpoch: 3}
	if !LeaseHeldBy(rec, "a", 3) {
		t.Fatal("expected lease held")
	}
	if LeaseHeldBy(rec, "b", 3) || LeaseHeldBy(rec, "a", 2) {
		t.Fatal("stale owner/epoch reported held")
	}
	if LeaseHeldBy(Record{State: StatePending}, "a", 3) {
		t.Fatal("pending record reported held")
	}
}

func TestEnqueueRejectsInvalidParamsWithoutTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	// Nil database and a panicking transaction runner prove validation runs
	// before any storage access.
	store := NewStore(nil, func(context.Context, func(context.Context, *sql.Tx) error) error {
		panic("must not run")
	})
	for name, params := range map[string]EnqueueParams{
		"empty kind":     {DedupKey: "k", ResourceType: "rt", ResourceID: "ri"},
		"empty dedup":    {Kind: "k", ResourceType: "rt", ResourceID: "ri"},
		"empty resource": {Kind: "k", DedupKey: "k"},
		"negative limit": {Kind: "k", DedupKey: "k", ResourceType: "rt", ResourceID: "ri", AttemptLimit: -1},
		"bad JSON":       {Kind: "k", DedupKey: "k", ResourceType: "rt", ResourceID: "ri", Payload: []byte("{nope")},
	} {
		if _, err := store.Enqueue(context.Background(), params); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
	valid, err := EnqueueParams{Kind: "k", DedupKey: "k", ResourceType: "rt", ResourceID: "ri"}.validated()
	if err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if valid.AttemptLimit != DefaultAttemptLimit {
		t.Fatalf("default attempt limit = %d, want %d", valid.AttemptLimit, DefaultAttemptLimit)
	}
	if string(valid.Payload) != "{}" {
		t.Fatalf("default payload = %q, want {}", valid.Payload)
	}
}
