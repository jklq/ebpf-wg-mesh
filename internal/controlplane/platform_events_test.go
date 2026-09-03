package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"
)

type memoryEnvironmentEvents struct {
	mu        sync.Mutex
	revisions map[string]int64
}

func newMemoryEnvironmentEvents() *memoryEnvironmentEvents {
	return &memoryEnvironmentEvents{revisions: make(map[string]int64)}
}

func (s *memoryEnvironmentEvents) currentEnvironmentEvent(_ context.Context, environmentID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision, ok := s.revisions[environmentID]; ok {
		return revision, nil
	}
	return initialEnvironmentRevision, nil
}

func (s *memoryEnvironmentEvents) publishEnvironmentEvent(_ context.Context, environmentID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision := s.revisions[environmentID]
	if revision == 0 {
		revision = initialEnvironmentRevision + 1
	} else {
		revision++
	}
	s.revisions[environmentID] = revision
	return revision, nil
}

func TestPlatformEventsWaitsForEnvironmentRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := NewPlatformEvents(newMemoryEnvironmentEvents(), 5*time.Millisecond)
	initial, err := events.Current(ctx, "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		index   int64
		changed bool
		err     error
	}, 1)
	go func() {
		index, changed, waitErr := events.Wait(ctx, "environment-1", initial, time.Second)
		result <- struct {
			index   int64
			changed bool
			err     error
		}{index: index, changed: changed, err: waitErr}
	}()

	got, err := events.Publish(ctx, "environment-2")
	if err != nil {
		t.Fatal(err)
	}
	if got <= initial {
		t.Fatalf("environment-2 revision did not advance: %d", got)
	}
	select {
	case <-result:
		t.Fatal("unrelated project woke blocking wait")
	case <-time.After(20 * time.Millisecond):
	}
	want, err := events.Publish(ctx, "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.changed || got.index != want {
			t.Fatalf("wait result = %+v, want changed index %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("environment event did not wake blocking wait")
	}
}

func TestPlatformEventsTimeoutReturnsNotModified(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := NewPlatformEvents(newMemoryEnvironmentEvents(), 5*time.Millisecond)
	initial, err := events.Current(ctx, "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	index, changed, err := events.Wait(ctx, "environment-1", initial, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if changed || index != initial {
		t.Fatalf("timeout = (%d, %v), want (%d, false)", index, changed, initial)
	}
}
