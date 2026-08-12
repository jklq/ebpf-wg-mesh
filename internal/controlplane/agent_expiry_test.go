package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAgentExpiryTrackerRenewsOneNodeDeadline(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		expired []string
	)
	fired := make(chan struct{}, 2)
	tracker := NewAgentExpiryTracker(context.Background(), 40*time.Millisecond, func(_ context.Context, agentID string, _ time.Time) error {
		mu.Lock()
		expired = append(expired, agentID)
		mu.Unlock()
		fired <- struct{}{}
		return nil
	})
	t.Cleanup(tracker.Close)

	tracker.Touch("node-1")
	time.Sleep(25 * time.Millisecond)
	tracker.Touch("node-1")
	tracker.Touch("node-2")

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("expiry callback did not run")
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("second expiry callback did not run")
	}
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(expired) != 2 {
		t.Fatalf("expected one expiry per node, got %v", expired)
	}
	seen := map[string]int{}
	for _, agentID := range expired {
		seen[agentID]++
	}
	if seen["node-1"] != 1 || seen["node-2"] != 1 {
		t.Fatalf("unexpected expiry callbacks: %v", expired)
	}
}

func TestAgentExpiryTrackerRestoresOverdueAgent(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	fired := make(chan string, 1)
	tracker := NewAgentExpiryTracker(context.Background(), time.Minute, func(_ context.Context, agentID string, _ time.Time) error {
		fired <- agentID
		return nil
	})
	t.Cleanup(tracker.Close)

	tracker.Restore("node-stale", now.Add(-2*time.Minute), now)
	select {
	case agentID := <-fired:
		if agentID != "node-stale" {
			t.Fatalf("expired agent = %q, want node-stale", agentID)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted overdue agent was not scheduled for expiry")
	}
}
