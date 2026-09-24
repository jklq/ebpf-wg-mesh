package logpipeline

import (
	"math/rand"
	"sync"
	"time"
)

// Backoff is an exponential retry backoff with jitter. Shippers reset
// it after every successful flush and sleep Next after failures, so a
// backend outage costs one quiet goroutine instead of a hot loop.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration

	mu       sync.Mutex
	failures int
	rng      *rand.Rand
}

// Next returns the delay before the next attempt.
func (b *Backoff) Next() time.Duration {
	initial := b.Initial
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}
	max := b.Max
	if max <= 0 {
		max = 30 * time.Second
	}
	b.mu.Lock()
	failures := b.failures
	b.failures++
	if b.rng == nil {
		b.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	jitter := time.Duration(b.rng.Int63n(int64(initial)))
	b.mu.Unlock()

	delay := initial * (1 << min(failures, 6))
	if delay > max || delay < 0 {
		delay = max
	}
	if delay+jitter > max {
		return max
	}
	return delay + jitter
}

// Reset clears the failure count after a successful attempt.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}
