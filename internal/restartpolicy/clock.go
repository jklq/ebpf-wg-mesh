package restartpolicy

import "time"

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

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
