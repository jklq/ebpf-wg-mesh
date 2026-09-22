package logs

import (
	"context"
	"testing"
)

func TestEmitEventCarriesStructuredAttributes(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	emitter := NewLogEmitter(store)
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
