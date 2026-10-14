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
}

type diskUsageCache struct {
	mu      sync.Mutex
	samples map[string]diskUsageSample
}

func (c *diskUsageCache) usage(id string, now time.Time, measure func() (int64, error)) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, exists := c.samples[id]
	if exists && now.Sub(sample.checkedAt) < diskUsageInterval {
		return sample.bytes, sample.err
	}
	written, err := measure()
	if err == nil {
		sample.bytes = written
	}
	sample.checkedAt, sample.err = now, err
	if c.samples == nil {
		c.samples = make(map[string]diskUsageSample)
	}
	c.samples[id] = sample
	return sample.bytes, sample.err
}

func (c *diskUsageCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.samples, id)
}
