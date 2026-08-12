package controlplane

import (
	"context"
	"sync"
	"time"
)

const maxPlatformBlockingWait = 5 * time.Minute

type platformEventState struct {
	index int64
	wake  chan struct{}
}

// PlatformEvents provides environment-scoped monotonic indexes and blocking waits.
// It is intentionally in-memory while the control plane is single-process.
type PlatformEvents struct {
	mu           sync.Mutex
	environments map[string]*platformEventState
}

func NewPlatformEvents() *PlatformEvents {
	return &PlatformEvents{environments: make(map[string]*platformEventState)}
}

func (e *PlatformEvents) state(environmentID string) *platformEventState {
	state := e.environments[environmentID]
	if state == nil {
		state = &platformEventState{index: 1, wake: make(chan struct{})}
		e.environments[environmentID] = state
	}
	return state
}

func (e *PlatformEvents) Publish(environmentID string) int64 {
	if e == nil || environmentID == "" {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.state(environmentID)
	state.index++
	close(state.wake)
	state.wake = make(chan struct{})
	return state.index
}

func (e *PlatformEvents) Current(environmentID string) int64 {
	if e == nil {
		return 1
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state(environmentID).index
}

// Wait returns the current index and whether it advanced beyond after.
func (e *PlatformEvents) Wait(ctx context.Context, environmentID string, after int64, timeout time.Duration) (int64, bool) {
	if e == nil || after <= 0 {
		return e.Current(environmentID), true
	}
	if timeout <= 0 || timeout > maxPlatformBlockingWait {
		timeout = maxPlatformBlockingWait
	}
	e.mu.Lock()
	state := e.state(environmentID)
	if state.index > after {
		index := state.index
		e.mu.Unlock()
		return index, true
	}
	wake := state.wake
	e.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-wake:
	}
	index := e.Current(environmentID)
	return index, index > after
}

func platformWaitDuration(seconds int32) time.Duration {
	if seconds <= 0 {
		return maxPlatformBlockingWait
	}
	return time.Duration(seconds) * time.Second
}
