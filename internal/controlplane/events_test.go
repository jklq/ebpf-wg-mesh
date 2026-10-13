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
	reads    int
}

func newMemoryGlobalRevision() *memoryGlobalRevision {
	return &memoryGlobalRevision{revision: initialEnvironmentRevision}
}

func (s *memoryGlobalRevision) currentGlobalRevision(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.revision, nil
}

func TestPlatformEventsSharePollingAcrossBlockingReaders(t *testing.T) {
	t.Parallel()
	store := newMemoryGlobalRevision()
	events := newPlatformEvents(store, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const readers = 40
	results := make(chan int64, readers)
	for range readers {
		go func() {
			index, changed, err := events.Wait(ctx, initialEnvironmentRevision, time.Second)
			if err != nil || !changed {
				results <- -1
				return
			}
			results <- index
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		events.mu.Lock()
		count := len(events.waiters)
		events.mu.Unlock()
		if count == readers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d readers subscribed", count)
		}
		time.Sleep(time.Millisecond)
	}
	store.mu.Lock()
	before := store.reads
	store.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	store.mu.Lock()
	reads := store.reads - before
	store.mu.Unlock()
	if reads >= readers*2 {
		t.Fatalf("blocking readers performed %d database polls; expected one shared poller", reads)
	}
	want := store.advance()
	for range readers {
		select {
		case index := <-results:
			if index != want {
				t.Fatalf("reader received index %d, want %d", index, want)
			}
		case <-time.After(time.Second):
			t.Fatal("revision did not wake every reader")
		}
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.waiters) != 0 || events.stopPolling != nil {
		t.Fatal("poller retained completed readers")
	}
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
	events := newPlatformEvents(store, 5*time.Millisecond)
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
	events := newPlatformEvents(newMemoryGlobalRevision(), 5*time.Millisecond)
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
