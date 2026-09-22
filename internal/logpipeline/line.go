package logpipeline

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// MaxLogLineBytes is the documented per-line size limit. Lines longer
// than this are truncated with truncated=true; the retained prefix is
// byte-exact.
const MaxLogLineBytes = 64 * 1024

// Attribute bounds for structured platform-event attributes.
const (
	MaxAttributesPerLine    = 16
	MaxAttributeKeyBytes    = 128
	MaxAttributeValueBytes  = 1024
	MaxEventNameBytes       = 64
	MaxStageNameBytes       = 64
	MaxStreamNameBytes      = 32
	MaxAttributeEntriesHard = 64
)

// Drop reasons reported in gap windows.
const (
	ReasonRateLimited    = "rate_limited"
	ReasonSpoolOverflow  = "spool_overflow"
	ReasonIngestOverflow = "ingest_overflow"
	ReasonCorruptSpool   = "corrupt_spool"
)

// TruncateLine enforces MaxLogLineBytes, returning the stored line and
// whether truncation happened.
func TruncateLine(line string) (string, bool) {
	if len(line) <= MaxLogLineBytes {
		return line, false
	}
	return line[:MaxLogLineBytes], true
}

// NewBootID returns a random per-process boot identifier. Agent line
// identities embed the boot ID so per-allocation sequence counters can
// restart at 1 on every boot without colliding with earlier lines.
func NewBootID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// AgentLineID builds the stable identity for one agent-emitted line.
// The caller assigns it at emit time and stores it in the spool
// record, so crash recovery replays the original identity and
// retried batches deduplicate server-side.
func AgentLineID(agentID, bootID, allocationID, stream string, seq uint64) string {
	return fmt.Sprintf("ag:%s:%s:%s:%s:%d", agentID, bootID, allocationID, stream, seq)
}

// BuilderLineID builds the stable identity for one builder-emitted
// line. The lease epoch scopes the per-build sequence: a retried
// report reuses the same IDs, while a new attempt after worker loss
// writes under a new epoch instead of clobbering the old attempt.
func BuilderLineID(builderID, buildID string, leaseEpoch int64, seq uint64) string {
	return fmt.Sprintf("bd:%s:%s:%d:%d", builderID, buildID, leaseEpoch, seq)
}

// SyntheticLineID returns a random identity for a control-plane
// synthetic line. The emitter assigns it when constructing the line
// input so ingest retries reuse it.
func SyntheticLineID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "sy:" + hex.EncodeToString(raw[:])
}

// NormalizeAttributes bounds platform-event attributes without
// interpreting customer data. Callers pass attributes only for lines
// the platform emitted; customer lines must pass nil and stay nil.
func NormalizeAttributes(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		if len(out) >= MaxAttributesPerLine {
			break
		}
		key = strings.TrimSpace(key)
		if key == "" || len(key) > MaxAttributeKeyBytes {
			continue
		}
		if len(value) > MaxAttributeValueBytes {
			value = value[:MaxAttributeValueBytes]
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeEvent bounds a platform event name. Unknown input becomes
// empty rather than passing through.
func NormalizeEvent(event string) string {
	event = strings.TrimSpace(event)
	if event == "" || len(event) > MaxEventNameBytes {
		return ""
	}
	for _, r := range event {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return ""
		}
	}
	return event
}

// NormalizeDropReason maps an arbitrary producer reason onto the
// known set so reads never surface unbounded cardinality.
func NormalizeDropReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case ReasonRateLimited:
		return ReasonRateLimited
	case ReasonSpoolOverflow:
		return ReasonSpoolOverflow
	case ReasonIngestOverflow:
		return ReasonIngestOverflow
	case ReasonCorruptSpool:
		return ReasonCorruptSpool
	default:
		return ReasonIngestOverflow
	}
}
