package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"
)

type memoryGlobalRevision struct {
	mu       sync.Mutex
	revision int64
}

func newMemoryGlobalRevision() *memoryGlobalRevision {
	return &memoryGlobalRevision{revision: initialEnvironmentRevision}
}

func (s *memoryGlobalRevision) currentGlobalRevision(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision, nil
}

func (s *memoryGlobalRevision) advance() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	return s.revision
}

func TestPlatformEventsWaitForGlobalRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newMemoryGlobalRevision()
	events := NewPlatformEvents(store, 5*time.Millisecond)
	initial, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		index   int64
		changed bool
		err     error
	}, 1)
	go func() {
		index, changed, waitErr := events.Wait(ctx, initial, time.Second)
		result <- struct {
			index   int64
			changed bool
			err     error
		}{index: index, changed: changed, err: waitErr}
	}()

	select {
	case <-result:
		t.Fatal("wait returned before the revision advanced")
	case <-time.After(20 * time.Millisecond):
	}

	want := store.advance()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.changed || got.index != want {
			t.Fatalf("wait result = %+v, want changed index %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("revision advance did not wake blocking wait")
	}
}

func TestPlatformEventsTimeoutReturnsNotModified(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := NewPlatformEvents(newMemoryGlobalRevision(), 5*time.Millisecond)
	initial, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index, changed, err := events.Wait(ctx, initial, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if changed || index != initial {
		t.Fatalf("timeout = (%d, %v), want (%d, false)", index, changed, initial)
	}
}
