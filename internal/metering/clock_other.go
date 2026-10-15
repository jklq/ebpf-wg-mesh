//go:build !linux

package metering

import (
	"time"

	"github.com/google/uuid"
)

type processClock struct {
	boot  string
	start time.Time
}

// SystemClock outside Linux treats each process as a new boot: there is no
// monotonic clock that survives a restart.
func SystemClock() (Clock, error) {
	return processClock{boot: uuid.NewString(), start: time.Now()}, nil
}

func (c processClock) Now() Instant {
	return Instant{Boot: c.boot, Mono: time.Since(c.start), Wall: time.Now()}
}
