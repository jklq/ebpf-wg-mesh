package agent

import (
	"sync"
	"time"
)

// Snapshot usage walks the writable filesystem. Bound its cost independently
// of reconciliation events; disk enforcement can lag writes by this interval.
const diskUsageInterval = 30 * time.Second

type diskUsageSample struct {
	checkedAt time.Time
	bytes     int64
	err       error
	pending   bool
}

type diskUsageCache struct {
	mu      sync.Mutex
	samples map[string]*diskUsageSample
}

func (c *diskUsageCache) usage(id string, now time.Time, measure func() (int64, error)) (int64, error) {
	c.mu.Lock()
	sample, exists := c.samples[id]
	if exists && (sample.pending || now.Sub(sample.checkedAt) < diskUsageInterval) {
		defer c.mu.Unlock()
		return sample.bytes, sample.err
	}
	if c.samples == nil {
		c.samples = make(map[string]*diskUsageSample)
	}
	if !exists {
		sample = &diskUsageSample{}
		c.samples[id] = sample
	}
	sample.pending = true
	c.mu.Unlock()
	written, err := measure()
	c.mu.Lock()
	defer c.mu.Unlock()
	// Removal invalidates any scan still in flight for the old snapshot.
	if c.samples[id] != sample {
		return 0, nil
	}
	if err == nil {
		sample.bytes = written
	}
	sample.checkedAt, sample.err = now, err
	sample.pending = false
	return sample.bytes, sample.err
}

func (c *diskUsageCache) cached(id string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sample := c.samples[id]; sample != nil {
		return sample.bytes
	}
	return 0
}

func (c *diskUsageCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.samples, id)
}
