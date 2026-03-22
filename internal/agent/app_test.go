package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func TestPeriodicReconcileLoopUsesLatestDesiredState(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu          sync.RWMutex
		latest      *agentv1.DesiredNodeState
		revisionsMu sync.Mutex
		revisions   []int64
		firstTick   = make(chan struct{}, 1)
		secondTick  = make(chan struct{}, 1)
	)

	mu.Lock()
	latest = &agentv1.DesiredNodeState{Revision: 1}
	mu.Unlock()

	go periodicReconcileLoop(ctx, 10*time.Millisecond, func() *agentv1.DesiredNodeState {
		mu.RLock()
		defer mu.RUnlock()
		if latest == nil {
			return nil
		}
		return latest
	}, func(state *agentv1.DesiredNodeState) {
		revisionsMu.Lock()
		revisions = append(revisions, state.GetRevision())
		count := len(revisions)
		revisionsMu.Unlock()
		switch count {
		case 1:
			select {
			case firstTick <- struct{}{}:
			default:
			}
		case 2:
			select {
			case secondTick <- struct{}{}:
			default:
			}
		}
	})

	select {
	case <-firstTick:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for first periodic reconcile")
	}

	mu.Lock()
	latest = &agentv1.DesiredNodeState{Revision: 2}
	mu.Unlock()

	select {
	case <-secondTick:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for second periodic reconcile")
	}

	revisionsMu.Lock()
	seen := append([]int64(nil), revisions...)
	revisionsMu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected at least two periodic reconciles, got %v", seen)
	}
	if seen[0] != 1 {
		t.Fatalf("expected first reconcile revision 1, got %v", seen)
	}
	if seen[1] != 2 {
		t.Fatalf("expected second reconcile to use latest revision 2, got %v", seen)
	}
}
