//go:build linux

package agent

import (
	"testing"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/typeurl/v2"
)

func TestNoteOOMEventDecodesTypedPayload(t *testing.T) {
	t.Parallel()
	engine := &containerdEngine{}

	payload, err := typeurl.MarshalAny(&eventsapi.TaskOOM{ContainerID: "platform-abc123"})
	if err != nil {
		t.Fatalf("MarshalAny TaskOOM: %v", err)
	}
	engine.noteOOMEvent(payload)
	if !engine.containerOOMKilled("platform-abc123") {
		t.Fatalf("containerOOMKilled(platform-abc123) = false, want true")
	}

	other, err := typeurl.MarshalAny(&eventsapi.TaskExit{ContainerID: "platform-nope"})
	if err != nil {
		t.Fatalf("MarshalAny TaskExit: %v", err)
	}
	engine.noteOOMEvent(other)
	if engine.containerOOMKilled("platform-nope") {
		t.Fatalf("containerOOMKilled(platform-nope) = true, want false: non-OOM events must be ignored")
	}

	engine.noteOOMEvent(nil)
}
