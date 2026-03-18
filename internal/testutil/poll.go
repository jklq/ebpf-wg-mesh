package testutil

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type PollConfig struct {
	Timeout  time.Duration
	Interval time.Duration
}

func (c PollConfig) withDefaults() PollConfig {
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	if c.Interval <= 0 {
		c.Interval = 50 * time.Millisecond
	}
	return c
}

func Poll(ctx context.Context, cfg PollConfig, predicate func(context.Context) (bool, error)) error {
	cfg = cfg.withDefaults()
	deadlineCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		ok, err := predicate(deadlineCtx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		select {
		case <-deadlineCtx.Done():
			if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("poll timeout after %s", cfg.Timeout)
			}
			return deadlineCtx.Err()
		case <-ticker.C:
		}
	}
}
