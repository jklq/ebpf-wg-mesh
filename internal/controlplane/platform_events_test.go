package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestPlatformEventsWaitsForEnvironmentRevision(t *testing.T) {
	t.Parallel()

	events := NewPlatformEvents()
	initial := events.Current("environment-1")
	result := make(chan struct {
		index   int64
		changed bool
	}, 1)
	go func() {
		index, changed := events.Wait(context.Background(), "environment-1", initial, time.Second)
		result <- struct {
			index   int64
			changed bool
		}{index: index, changed: changed}
	}()

	if got := events.Publish("environment-2"); got <= initial {
		t.Fatalf("environment-2 revision did not advance: %d", got)
	}
	select {
	case <-result:
		t.Fatal("unrelated project woke blocking wait")
	case <-time.After(20 * time.Millisecond):
	}
	want := events.Publish("environment-1")
	select {
	case got := <-result:
		if !got.changed || got.index != want {
			t.Fatalf("wait result = %+v, want changed index %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("environment event did not wake blocking wait")
	}
}

func TestPlatformEventsTimeoutReturnsNotModified(t *testing.T) {
	t.Parallel()

	events := NewPlatformEvents()
	initial := events.Current("environment-1")
	index, changed := events.Wait(context.Background(), "environment-1", initial, time.Millisecond)
	if changed || index != initial {
		t.Fatalf("timeout = (%d, %v), want (%d, false)", index, changed, initial)
	}
}
