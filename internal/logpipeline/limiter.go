package logpipeline

import (
	"sync"
	"time"
)

// Limiter is a per-key token bucket. Agents use one key per
// allocation, builders one key per build, and the control plane one
// key per allocation as an ingest guard. Drops are counted per key
// so producers can persist them as explicit read gaps. The key table
// is bounded and recycles its least recently used bucket, so a
// long-lived process never permanently rejects a new key.
type Limiter struct {
	ratePerSec float64
	burst      float64
	maxKeys    int
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket
	dropped map[string]uint64
}

// tokenBucket tracks one key's allowance. Tokens accrue at rate per
// second up to burst; each allowed line spends one token.
type tokenBucket struct {
	tokens  float64
	updated time.Time
}

// NewLimiter builds a limiter allowing ratePerSec lines per second
// per key with bursts of burst lines. A non-positive rate allows
// everything (no limiting, no counting).
func NewLimiter(ratePerSec float64, burst int) *Limiter {
	return newLimiter(ratePerSec, burst, 4096, time.Now)
}

func newLimiter(ratePerSec float64, burst, maxKeys int, now func() time.Time) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &Limiter{
		ratePerSec: ratePerSec,
		burst:      float64(burst),
		maxKeys:    maxKeys,
		now:        now,
		buckets:    make(map[string]*tokenBucket),
		dropped:    make(map[string]uint64),
	}
}

// Allow reports whether key may emit one line now, spending a token.
// Denied lines are counted and must surface as read gaps.
func (l *Limiter) Allow(key string) bool {
	if l == nil || l.ratePerSec <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[key]
	if !ok {
		for len(l.buckets) >= l.maxKeys {
			l.evictOldestLocked()
		}
		bucket = &tokenBucket{tokens: l.burst, updated: now}
		l.buckets[key] = bucket
	}
	elapsed := now.Sub(bucket.updated).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * l.ratePerSec
		if bucket.tokens > l.burst {
			bucket.tokens = l.burst
		}
		bucket.updated = now
	}
	if bucket.tokens < 1 {
		l.dropped[key]++
		return false
	}
	bucket.tokens--
	return true
}

// evictOldestLocked recycles the least recently used bucket so a key
// is never permanently rejected just because the process outlived the
// key cap. Evicted keys return to a full burst, which for a producer
// guard is the right failure direction.
func (l *Limiter) evictOldestLocked() {
	var victim string
	var oldest time.Time
	first := true
	for key, bucket := range l.buckets {
		if first || bucket.updated.Before(oldest) {
			victim, oldest, first = key, bucket.updated, false
		}
	}
	if !first {
		delete(l.buckets, victim)
	}
}

// DroppedSince returns the denied count for key without clearing it.
func (l *Limiter) DroppedSince(key string) uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped[key]
}

// DrainDrops returns per-key denied counts accumulated since the
// last drain and clears them.
func (l *Limiter) DrainDrops() map[string]uint64 {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dropped) == 0 {
		return nil
	}
	out := l.dropped
	l.dropped = make(map[string]uint64)
	return out
}

// Keys returns the number of tracked keys.
func (l *Limiter) Keys() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
