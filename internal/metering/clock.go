package metering

import (
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

// Clock reads the host's boot identity, a monotonic clock that keeps
// counting across agent restarts within one boot, and the wall clock.
type Clock interface {
	Now() Instant
}

// Instant is one clock reading.
type Instant struct {
	Boot string
	// Mono counts from host boot and includes suspend time.
	Mono time.Duration
	Wall time.Time
}

// stepTolerance is how far the wall clock may drift from the producer
// timeline before the collector treats it as a step.
const stepTolerance = time.Second

// timeline maps monotonic readings to UTC milliseconds. Elapsed time always
// comes from the monotonic clock; the wall clock only anchors it. A forward
// wall-clock step re-anchors the timeline and flags the interval that spans
// it. A backward step never moves the timeline back, so a resource's
// intervals cannot overlap; intervals are flagged while the wall clock
// trails the timeline.
type timeline struct {
	Boot       string `json:"boot"`
	AnchorMono int64  `json:"anchor_mono_ns"`
	AnchorMS   int64  `json:"anchor_ms"`
	LastMS     int64  `json:"last_ms"`
	Steps      uint64 `json:"steps"`
	Skewed     bool   `json:"skewed"`
}

// point is one sample position on the producer timeline.
type point struct {
	Boot   string `json:"boot"`
	Mono   int64  `json:"mono_ns"`
	MS     int64  `json:"ms"`
	Steps  uint64 `json:"steps"`
	Skewed bool   `json:"skewed,omitempty"`
}

func (t *timeline) at(now Instant) point {
	wall := now.Wall.UnixMilli()
	mono := int64(now.Mono)
	if now.Boot != t.Boot {
		anchor := wall
		t.Skewed = false
		if anchor < t.LastMS {
			anchor, t.Skewed = t.LastMS, true
		}
		t.Boot, t.AnchorMono, t.AnchorMS = now.Boot, mono, anchor
	}
	derived := t.AnchorMS + (mono-t.AnchorMono)/int64(time.Millisecond)
	switch deviation := wall - derived; {
	case deviation > stepTolerance.Milliseconds():
		t.AnchorMono, t.AnchorMS, t.Skewed = mono, wall, false
		t.Steps++
		derived = wall
	case deviation < -stepTolerance.Milliseconds():
		t.Skewed = true
	default:
		t.Skewed = false
	}
	if derived < t.LastMS {
		derived = t.LastMS
	}
	t.LastMS = derived
	return point{Boot: now.Boot, Mono: mono, MS: derived, Steps: t.Steps, Skewed: t.Skewed}
}

// interval returns the duration and clock quality of the interval between
// two points of one boot. The duration equals the UTC distance unless the
// timeline was re-anchored in between; then it is the monotonic elapsed time.
func interval(from, to point) (int64, uint32) {
	var quality uint32
	if from.Skewed || to.Skewed {
		quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_CLOCK_SKEW)
	}
	if from.Steps != to.Steps && from.Boot == to.Boot && to.Mono >= from.Mono {
		quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_CLOCK_STEP)
		return (to.Mono - from.Mono) / int64(time.Millisecond), quality
	}
	return to.MS - from.MS, quality
}
