package durablework

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Work states. Terminal states are StateSucceeded, StateFailed, and StateDead.
const (
	StatePending   = "pending"
	StateLeased    = "leased"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateDead      = "dead"
)

// Retry defaults applied when the caller leaves the corresponding field zero.
const (
	DefaultAttemptLimit = 10
	DefaultBaseDelay    = 5 * time.Second
	DefaultMaxDelay     = 5 * time.Minute
)

// MaxLastErrorRunes bounds the sanitized error text stored on a record.
const MaxLastErrorRunes = 2048

var (
	// ErrLeaseLost reports that a heartbeat, completion, or failure named a
	// stale (owner, epoch) pair: the lease expired and another owner took the
	// record over, or the record left the leased state. The caller must drop
	// the work; it must not retry the commit.
	ErrLeaseLost = errors.New("durable work lease lost")
)

// Record is one durable work item.
type Record struct {
	ID             string
	Kind           string
	DedupKey       string
	ResourceType   string
	ResourceID     string
	State          string
	AttemptCount   int64
	AttemptLimit   int64
	OwnerID        string
	OwnerEpoch     int64
	LeaseExpiresAt sql.NullTime
	LastError      string
	AvailableAt    time.Time
	Payload        []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    sql.NullTime
}

// Terminal reports whether the record is in succeeded, failed, or dead state.
func (r Record) Terminal() bool {
	switch r.State {
	case StateSucceeded, StateFailed, StateDead:
		return true
	default:
		return false
	}
}

// EnqueueParams describes one work record to enqueue.
type EnqueueParams struct {
	Kind         string
	DedupKey     string
	ResourceType string
	ResourceID   string
	// Payload is caller-defined JSON carried opaquely by the queue. Empty
	// means "{}".
	Payload []byte
	// AttemptLimit bounds total claims including takeovers. Zero means
	// DefaultAttemptLimit.
	AttemptLimit int64
	// AvailableAt delays the first claim. Zero means immediately.
	AvailableAt time.Time
}

func (p EnqueueParams) validated() (EnqueueParams, error) {
	p.Kind = strings.TrimSpace(p.Kind)
	p.DedupKey = strings.TrimSpace(p.DedupKey)
	p.ResourceType = strings.TrimSpace(p.ResourceType)
	p.ResourceID = strings.TrimSpace(p.ResourceID)
	if p.Kind == "" {
		return EnqueueParams{}, errors.New("durable work kind is required")
	}
	if p.DedupKey == "" {
		return EnqueueParams{}, errors.New("durable work dedup key is required")
	}
	if p.ResourceType == "" || p.ResourceID == "" {
		return EnqueueParams{}, errors.New("durable work resource type and id are required")
	}
	if p.AttemptLimit < 0 {
		return EnqueueParams{}, errors.New("durable work attempt limit must not be negative")
	}
	if p.AttemptLimit == 0 {
		p.AttemptLimit = DefaultAttemptLimit
	}
	if len(bytes.TrimSpace(p.Payload)) == 0 {
		p.Payload = []byte("{}")
	} else if !json.Valid(p.Payload) {
		return EnqueueParams{}, errors.New("durable work payload must be valid JSON")
	}
	return p, nil
}

// FailOptions controls how Fail dispositions a leased record.
type FailOptions struct {
	// Retryable requeues the record when attempts remain and dead-letters it
	// otherwise. A non-retryable failure moves the record to failed at once.
	Retryable bool
	// RetryAfter overrides the computed backoff for this failure. Zero means
	// exponential backoff from the record's attempt count. The override is
	// still jittered.
	RetryAfter time.Duration
	// BaseDelay and MaxDelay bound the computed backoff. Zero means the
	// corresponding default.
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

func (o FailOptions) bounds() (base, max time.Duration) {
	base, max = o.BaseDelay, o.MaxDelay
	if base <= 0 {
		base = DefaultBaseDelay
	}
	if max <= 0 {
		max = DefaultMaxDelay
	}
	if max < base {
		max = base
	}
	return base, max
}

// RetryDelay returns the jittered backoff before the next attempt. Attempt is
// the 1-based attempt count that just failed; the delay doubles per attempt
// from base up to max, then jitters by up to twenty percent in either
// direction.
func RetryDelay(attempt int64, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultBaseDelay
	}
	if max <= 0 {
		max = DefaultMaxDelay
	}
	if max < base {
		max = base
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := int64(1); i < attempt && delay < max; i++ {
		delay *= 2
		if delay >= max {
			delay = max
			break
		}
	}
	return jitter(delay)
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	spread := base / 5
	if spread <= 0 {
		return base
	}
	// rand.Int64N panics on n <= 0; spread > 0 here.
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

// SanitizeError renders err as bounded single-line stored text. Empty errors
// render as "". NUL bytes (rejected by the database text encoding) are
// stripped and the text is truncated to MaxLastErrorRunes.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return SanitizeText(err.Error())
}

// SanitizeText bounds arbitrary text for storage on a work record.
func SanitizeText(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > MaxLastErrorRunes {
		runes = runes[:MaxLastErrorRunes]
	}
	return strings.TrimSpace(string(runes))
}

// LeaseHeldBy reports whether rec is leased to owner at epoch. It is a
// pure helper for handlers that keep the claimed record alongside the
// owner they claimed with; the database CAS is the authority, not this.
func LeaseHeldBy(rec Record, ownerID string, epoch int64) bool {
	return rec.State == StateLeased && rec.OwnerID == ownerID && rec.OwnerEpoch == epoch
}

func leaseLost(id, ownerID string, epoch int64) error {
	return fmt.Errorf("%w: work %s owner %s epoch %d", ErrLeaseLost, id, ownerID, epoch)
}
