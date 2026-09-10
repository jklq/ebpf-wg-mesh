package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDiscoveryCandidatesPreferOwnerPinWithoutChangingSet(t *testing.T) {
	got := discoveryCandidates(" owner:9443 ", []string{"seed-a:9443", "owner:9443", "replica:9443", "seed-a:9443"}, nil)
	want := []string{"owner:9443", "seed-a:9443", "replica:9443"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("discovery candidates = %#v, want %#v", got, want)
	}
}

func TestDiscoveryCandidatesSkipQuarantinedOwnerEvenIfPinned(t *testing.T) {
	got := discoveryCandidates("owner:9443", []string{"seed-a:9443", "owner:9443", "replica:9443"}, map[string]time.Time{
		"owner:9443": time.Now().Add(time.Minute),
	})
	want := []string{"seed-a:9443", "replica:9443"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("discovery candidates = %#v, want %#v", got, want)
	}
}

func TestLiveOwnerRedirectParsing(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, agentv1.LiveOwnerRedirectPrefix+"owner.example:9443")
	got, ok := liveOwnerAddr(err)
	if !ok || got != "owner.example:9443" {
		t.Fatalf("liveOwnerAddr = %q, %t", got, ok)
	}

	if got, ok := liveOwnerAddr(status.Error(codes.FailedPrecondition, "not a redirect")); ok || got != "" {
		t.Fatalf("unexpected redirect parse result %q, %t", got, ok)
	}

	wrapped := fmt.Errorf("issue certificate: %w", status.Error(codes.FailedPrecondition, agentv1.LiveOwnerRedirectPrefix+"owner.example:9443"))
	if got, ok := liveOwnerAddr(wrapped); !ok || got != "owner.example:9443" {
		t.Fatalf("wrapped liveOwnerAddr = %q, %t", got, ok)
	}
}

func TestReplicaRetryabilityOnlyIncludesTransportFailures(t *testing.T) {
	if !isReplicaRetryable(status.Error(codes.Unavailable, "replica is down")) {
		t.Fatal("Unavailable should walk the discovery set")
	}
	if !isReplicaRetryable(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded should walk the discovery set")
	}
	if !isReplicaRetryable(io.EOF) {
		t.Fatal("EOF should walk the discovery set")
	}
	if isReplicaRetryable(status.Error(codes.PermissionDenied, "not this agent")) {
		t.Fatal("authorization failure should not walk the discovery set")
	}
	if isReplicaRetryable(errors.New("application failure")) {
		t.Fatal("unknown application failure should not walk the discovery set")
	}
	if got := forgetDeadPin("owner:9443", "owner:9443", status.Error(codes.Unavailable, "down")); got != "" {
		t.Fatalf("dead pin = %q, want empty", got)
	}
	if got := forgetDeadPin("owner:9443", "owner:9443", status.Error(codes.PermissionDenied, "rejected")); got != "owner:9443" {
		t.Fatalf("live pin after application error = %q, want owner:9443", got)
	}
}

func TestFollowLiveOwnerPinsReadyOwnerAndRejectsQuarantinedOwner(t *testing.T) {
	app := &App{}
	redirect := agentv1.LiveOwnerRedirect("owner:9443")
	if err := app.followLiveOwner(redirect); !errors.Is(err, errRedirectOwner) {
		t.Fatalf("follow live owner: %v", err)
	}
	if app.controlPlaneAddr != "owner:9443" {
		t.Fatalf("pin = %q", app.controlPlaneAddr)
	}

	app.quarantineOwner("owner:9443")
	app.controlPlaneAddr = ""
	if err := app.followLiveOwner(redirect); !errors.Is(err, errOwnerQuarantined) {
		t.Fatalf("quarantined owner: %v", err)
	}
	if app.controlPlaneAddr != "" {
		t.Fatalf("quarantined redirect re-pinned %q", app.controlPlaneAddr)
	}
}

func TestFollowLiveOwnerDoesNotRefreshQuarantine(t *testing.T) {
	oldCooldown := deadOwnerCooldown
	deadOwnerCooldown = 50 * time.Millisecond
	t.Cleanup(func() { deadOwnerCooldown = oldCooldown })

	app := &App{}
	app.quarantineOwner("owner:9443")
	until := app.deadOwners["owner:9443"]
	redirect := agentv1.LiveOwnerRedirect("owner:9443")
	for i := 0; i < 5; i++ {
		if err := app.followLiveOwner(redirect); !errors.Is(err, errOwnerQuarantined) {
			t.Fatalf("quarantined owner: %v", err)
		}
	}
	if app.deadOwners["owner:9443"] != until {
		t.Fatal("quarantine deadline was refreshed by later redirects")
	}

	time.Sleep(60 * time.Millisecond)
	if err := app.followLiveOwner(redirect); !errors.Is(err, errRedirectOwner) {
		t.Fatalf("follow recovered owner: %v", err)
	}
	if app.controlPlaneAddr != "owner:9443" {
		t.Fatalf("pin after cooldown = %q", app.controlPlaneAddr)
	}
}
