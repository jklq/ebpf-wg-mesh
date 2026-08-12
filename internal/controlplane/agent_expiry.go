package controlplane

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const agentExpiryRetryDelay = 5 * time.Second

type agentExpiryFunc func(context.Context, string, time.Time) error

// AgentExpiryTracker maintains one deadline per live agent. Expiry work is
// therefore scoped to the node whose heartbeat stopped instead of scanning all
// services on a fixed interval.
type AgentExpiryTracker struct {
	ctx     context.Context
	timeout time.Duration
	expire  agentExpiryFunc

	mu         sync.Mutex
	generation uint64
	timers     map[string]agentExpiryTimer
	closed     bool
}

type agentExpiryTimer struct {
	generation uint64
	timer      *time.Timer
}

func NewAgentExpiryTracker(ctx context.Context, timeout time.Duration, expire agentExpiryFunc) *AgentExpiryTracker {
	return &AgentExpiryTracker{
		ctx:     ctx,
		timeout: timeout,
		expire:  expire,
		timers:  make(map[string]agentExpiryTimer),
	}
}

func (t *AgentExpiryTracker) Touch(agentID string) {
	if t == nil || agentID == "" {
		return
	}
	t.schedule(agentID, t.timeout)
}

// Restore schedules an agent from its persisted heartbeat timestamp. This is
// used after a control-plane restart so agents that died before the restart do
// not disappear from the in-memory expiry schedule.
func (t *AgentExpiryTracker) Restore(agentID string, lastSeen, now time.Time) {
	if t == nil || agentID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	delay := lastSeen.Add(t.timeout).Sub(now)
	if delay < 0 {
		delay = 0
	}
	t.schedule(agentID, delay)
}

func (t *AgentExpiryTracker) schedule(agentID string, delay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if previous, ok := t.timers[agentID]; ok {
		previous.timer.Stop()
	}
	t.generation++
	generation := t.generation
	timer := time.AfterFunc(delay, func() {
		t.fire(agentID, generation)
	})
	t.timers[agentID] = agentExpiryTimer{generation: generation, timer: timer}
}

func (t *AgentExpiryTracker) fire(agentID string, generation uint64) {
	t.mu.Lock()
	current, ok := t.timers[agentID]
	if t.closed || !ok || current.generation != generation {
		t.mu.Unlock()
		return
	}
	delete(t.timers, agentID)
	t.mu.Unlock()

	cutoff := time.Now().UTC().Add(-t.timeout)
	if err := t.expire(t.ctx, agentID, cutoff); err != nil && t.ctx.Err() == nil {
		slog.Warn("targeted agent expiry evaluation failed", "agent_id", agentID, "error", err)
		t.schedule(agentID, agentExpiryRetryDelay)
	}
}

func (t *AgentExpiryTracker) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for agentID, item := range t.timers {
		item.timer.Stop()
		delete(t.timers, agentID)
	}
}
