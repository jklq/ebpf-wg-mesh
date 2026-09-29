package logpipeline

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLogLineBytes is the per-line size limit. Longer lines truncate with
// truncated=true; the retained prefix is byte-exact.
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

// TruncateLine enforces MaxLogLineBytes, returning the stored line and whether
// it truncated. The cut lands on a rune boundary: protobuf rejects invalid
// UTF-8, so a mid-rune cut would silently drop the line.
func TruncateLine(line string) (string, bool) {
	if len(line) <= MaxLogLineBytes {
		return line, false
	}
	cut := MaxLogLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut], true
}

// NewBootID returns a random per-process boot identifier, embedded in agent
// line IDs so per-allocation sequence counters restart at 1 each boot.
func NewBootID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// AgentLineID builds the stable identity for one agent-emitted line. Assigned
// at emit time and stored in the spool, so crash recovery replays it and
// retries deduplicate server-side.
func AgentLineID(agentID, bootID, allocationID, stream string, seq uint64) string {
	return fmt.Sprintf("ag:%s:%s:%s:%s:%d", agentID, bootID, allocationID, stream, seq)
}

// BuilderLineID builds the stable identity for one builder-emitted line. The
// lease epoch scopes the sequence: retried reports reuse IDs, while a new
// attempt after worker loss writes under a new epoch.
func BuilderLineID(builderID, buildID string, leaseEpoch int64, seq uint64) string {
	return fmt.Sprintf("bd:%s:%s:%d:%d", builderID, buildID, leaseEpoch, seq)
}

// StableEventID derives a platform event's identity from content-stable facts
// (ownership, rollout generation, observation window) so re-observed state
// collapses to one row. Facts distinguishing genuine events must be included;
// wall-clock report times must not.
func StableEventID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sy:" + hex.EncodeToString(h.Sum(nil)[:16])
}

// SyntheticLineID returns a random identity for a control-plane synthetic line
// with no content-stable facts. Assigned at construction so retries reuse it.
func SyntheticLineID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "sy:" + hex.EncodeToString(raw[:])
}

// NormalizeAttributes bounds platform-emitted event attributes without
// interpreting customer data. Customer lines must pass nil.
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

// NormalizeEvent bounds a platform event name; unknown input becomes empty.
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

// NormalizeDropReason maps an arbitrary producer reason onto the known set,
// bounding read cardinality.
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
