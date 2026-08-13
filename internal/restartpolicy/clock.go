package restartpolicy

import "time"

// Clock is an injectable source of UTC times for restart decisions.
type Clock interface {
	Now() time.Time
}

// SystemClock returns the current UTC time.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a deterministic clock for tests.
type ManualClock struct {
	now time.Time
}

func NewManualClock(now time.Time) *ManualClock {
	return &ManualClock{now: now.UTC()}
}

func (c *ManualClock) Now() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.now
}

func (c *ManualClock) Set(now time.Time) {
	c.now = now.UTC()
}

func (c *ManualClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}
