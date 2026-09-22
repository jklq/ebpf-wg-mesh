package logs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEmitEventCarriesStructuredAttributes(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	emitter := NewLogEmitter(store, nil)
	emitter.EmitEvent(context.Background(),
		ServiceScope{EnvironmentID: "env-1", ServiceID: "svc-1", RolloutGeneration: 3},
		LogTypeBuild, "build-1", EventBuildFinished, "done\n",
		map[string]string{"outcome": "succeeded"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 1 {
		t.Fatalf("expected 1 emitted line, got %d", len(store.lines))
	}
	in := store.lines[0]
	if in.Event != EventBuildFinished || in.Stage != StageBuild || in.BuildID != "build-1" {
		t.Fatalf("structured fields mangled: event=%q stage=%q build=%q", in.Event, in.Stage, in.BuildID)
	}
	if in.Attributes["outcome"] != "succeeded" {
		t.Fatalf("attributes dropped: %+v", in.Attributes)
	}
	if in.Line != "done" {
		t.Fatalf("line not exact: %q", in.Line)
	}
	if in.ID == "" {
		t.Fatal("synthetic event line must carry a stable identity for retry dedup")
	}
	if in.RolloutGeneration != 3 {
		t.Fatalf("rollout generation = %d, want 3", in.RolloutGeneration)
	}
}

// Platform events ride the async ingest queue: a ClickHouse outage at
// emission time retries instead of dropping the event, and shutdown
// drains queued events instead of dying with the request context.
func TestLogEmitterEventsSurviveBackendOutageAndShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse down"), failUntil: time.Now().Add(300 * time.Millisecond)}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 4, ShutdownGrace: 10 * time.Second})
	emitter := NewLogEmitter(store, ingester)

	emitter.EmitEvent(context.Background(), ServiceScope{EnvironmentID: "env-1", ServiceID: "svc-1"}, LogTypeDeploy, "", EventDeployStarted, "deployment started", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ingester.Run(ctx)
	}()

	// The first event must retry through the outage and land once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.lines)
		store.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event was not retried into the store: %d lines", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// An event accepted right before shutdown must drain at shutdown
	// rather than dying with the canceled context.
	emitter.EmitEvent(context.Background(), ServiceScope{EnvironmentID: "env-1", ServiceID: "svc-1"}, LogTypeDeploy, "", EventBuildFinished, "deployment finished", nil)
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 2 {
		t.Fatalf("shutdown delivered %d of 2 events", len(store.lines))
	}
	if store.lines[0].Event != EventDeployStarted || store.lines[1].Event != EventBuildFinished {
		t.Fatalf("events mangled in transit: %+v", store.lines)
	}
	if store.lines[0].ServiceID != "svc-1" {
		t.Fatalf("event scope mangled: %+v", store.lines[0])
	}
}
