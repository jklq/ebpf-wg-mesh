package delivery

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func startLive(t *testing.T) *Live {
	t.Helper()
	l := NewLive()
	if err := l.become(context.Background(), func(_ context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
		return fn(nil, journal.DurableState{ClusterID: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.resign)
	return l
}

func TestLiveHeartbeatResetsTimerAndStaleSession(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.Heartbeat("agent", "s1", true); err != nil {
		t.Fatal(err)
	}
	if err := l.Heartbeat("agent", "old", true); !errors.Is(err, ErrStaleAgentSession) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if !l.Admitted("agent") {
		t.Fatal("empty inventory should admit")
	}
}

func TestLiveSessionReplacementInvalidatesObservations(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.AcceptReport("agent", "s1", 1, true); err != nil {
		t.Fatal(err)
	}
	changed, err := l.RecordObservation(AllocationObservation{
		AllocationID: "alloc", RolloutGeneration: 1, Phase: "Healthy", Healthy: true,
		AgentID: "agent", SessionID: "s1", Sequence: 1,
	})
	if err != nil || !changed {
		t.Fatalf("record: %v %v", changed, err)
	}
	if err := l.BeginSession("agent", "s2", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Observation("alloc", 1); ok {
		t.Fatal("replacement left previous observation")
	}
	if err := l.Heartbeat("agent", "s1", true); !errors.Is(err, ErrStaleAgentSession) {
		t.Fatalf("old session heartbeat: %v", err)
	}
}

func TestLiveUnchangedObservationDoesNotTriggerWork(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.AcceptReport("agent", "s1", 1, true); err != nil {
		t.Fatal(err)
	}
	obs := AllocationObservation{AllocationID: "alloc", RolloutGeneration: 1, Phase: "Healthy", Healthy: true, AgentID: "agent", SessionID: "s1", Sequence: 1}
	changed, err := l.RecordObservation(obs)
	if err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if err := l.AcceptReport("agent", "s1", 2, true); err != nil {
		t.Fatal(err)
	}
	obs.Sequence = 2
	changed, err = l.RecordObservation(obs)
	if err != nil || changed {
		t.Fatalf("unchanged: %v %v", changed, err)
	}
}

func TestLiveExpiryUpdatesReachability(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if !l.Admitted("agent") {
		t.Fatal("expected admitted")
	}
	l.ExpireForTest("agent")
	session, ok := l.Session("agent")
	if !ok || session.Reachable {
		t.Fatalf("expected unreachable: %+v", session)
	}
	if l.Admitted("agent") {
		t.Fatal("expired agent still schedulable")
	}
}

func TestLiveInventoryMustReconcileBeforeAdmission(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, []string{"alloc-1"}, true); err != nil {
		t.Fatal(err)
	}
	if l.Admitted("agent") {
		t.Fatal("admitted before inventory")
	}
	if err := l.BeginSession("agent", "s2", []string{"alloc-1"}, []string{"alloc-1"}, true); err != nil {
		t.Fatal(err)
	}
	if !l.Admitted("agent") {
		t.Fatal("reconciled agent not admitted")
	}
}

func TestLiveOverlayAbsentHasNoLastContact(t *testing.T) {
	seen := time.Now().UTC()
	got := overlayAgentAbsent(AgentRecord{
		ID: "agent", LifecycleState: AgentStateActive, LastSeenAt: seen,
	})
	if got.LifecycleState != AgentStateUnavailable {
		t.Fatalf("state = %s", got.LifecycleState)
	}
	if got.StateBeforeUnavailable != AgentStateActive {
		t.Fatalf("admin state = %s", got.StateBeforeUnavailable)
	}
	if !got.LastSeenAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("last seen = %v, want unknown", got.LastSeenAt)
	}
}

func TestLiveOwnerRedirectMessage(t *testing.T) {
	msg := LiveOwnerRedirectMessage("127.0.0.1:9")
	addr, ok := ParseLiveOwnerRedirect(msg)
	if !ok || addr != "127.0.0.1:9" {
		t.Fatalf("parse %q: %q %v", msg, addr, ok)
	}
}
