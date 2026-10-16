package publicapi

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimiter is a per-token limiter with two windows, mirroring common
// public API practice: a sustained hourly quota and a short burst rate.
// Counters are per control-plane replica; with N replicas a token can use
// up to N times the quota. Quotas are protective, not billing meters.
type rateLimiter struct {
	perHour int
	burst   int // allowed requests per second, sustained by refill

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	// hourly window
	windowStart time.Time
	hourCount   int
	// burst bucket, refilled at burst tokens/second up to burst
	tokens float64
	last   time.Time
}

func newRateLimiter(perHour, burstPerSecond int) *rateLimiter {
	return &rateLimiter{perHour: perHour, burst: burstPerSecond, buckets: map[string]*tokenBucket{}}
}

type rateDecision struct {
	allowed   bool
	limit     int
	remaining int
	reset     time.Time
	retryIn   time.Duration
}

func (l *rateLimiter) allow(key string, now time.Time) rateDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{windowStart: now, tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}
	if now.Sub(b.windowStart) >= time.Hour {
		b.windowStart, b.hourCount = now, 0
	}
	// Opportunistically drop idle buckets so the map tracks live tokens.
	if len(l.buckets) > 4096 {
		for k, other := range l.buckets {
			if now.Sub(other.last) > 2*time.Hour {
				delete(l.buckets, k)
			}
		}
	}
	reset := b.windowStart.Add(time.Hour)
	if b.hourCount >= l.perHour {
		return rateDecision{limit: l.perHour, reset: reset, retryIn: time.Until(reset)}
	}
	b.tokens = min(float64(l.burst), b.tokens+now.Sub(b.last).Seconds()*float64(l.burst))
	b.last = now
	if b.tokens < 1 {
		deficit := 1 - b.tokens
		return rateDecision{limit: l.perHour, remaining: l.perHour - b.hourCount, reset: reset,
			retryIn: time.Duration(deficit / float64(l.burst) * float64(time.Second))}
	}
	b.tokens--
	b.hourCount++
	return rateDecision{allowed: true, limit: l.perHour, remaining: l.perHour - b.hourCount, reset: reset}
}

// writeRateHeaders emits the limit headers on every API response.
func (l *rateLimiter) writeRateHeaders(w http.ResponseWriter, d rateDecision) {
	h := w.Header()
	h.Set("RateLimit-Policy", fmt.Sprintf(`"default";q=%d;w=3600`, l.perHour))
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(max(d.remaining, 0)))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(d.reset.Unix(), 10))
	if !d.allowed && d.retryIn > 0 {
		h.Set("Retry-After", strconv.Itoa(int(d.retryIn.Seconds())+1))
	}
}
