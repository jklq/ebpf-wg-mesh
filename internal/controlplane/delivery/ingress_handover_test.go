package delivery

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestIngressHandoverRequiresEveryServingObservation(t *testing.T) {
	l := NewLive()
	state := journal.DurableState{
		ClusterID: "test", LogIndex: 1,
		Domains: map[string]journal.Domain{"web.example": {Hostname: "web.example", ServiceID: "web", TargetPort: 8080}},
		Assignments: map[string]journal.Assignment{
			"a":         {ID: "a", ServiceID: "web", AgentID: "agent", DesiredRolloutGeneration: 2, RolloutState: AllocationRolloutServing},
			"b":         {ID: "b", ServiceID: "web", AgentID: "agent", DesiredRolloutGeneration: 2, RolloutState: AllocationRolloutServing},
			"candidate": {ID: "candidate", ServiceID: "web", AgentID: "agent", RolloutState: AllocationRolloutStarting},
			"unrouted":  {ID: "unrouted", ServiceID: "internal", AgentID: "other", RolloutState: AllocationRolloutServing},
		},
	}
	become := func() {
		t.Helper()
		if err := l.become(context.Background(), func(_ context.Context, fn func(*sql.Tx, *journal.Projection) error) error {
			return fn(nil, journal.NewProjection(state))
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertPending := func() {
		t.Helper()
		l.SetPublishing(true) // Successful lease renewal cannot bypass the gate.
		if l.Publishing() {
			t.Fatal("publication allowed with incomplete observations")
		}
		if _, _, err := l.IngressState(); !errors.Is(err, ErrIngressObservationsPending) {
			t.Fatalf("IngressState error = %v", err)
		}
	}
	observe := func(id string, generation int64, healthy bool) {
		t.Helper()
		if _, err := l.RecordObservation(AllocationObservation{
			AllocationID: id, RolloutGeneration: generation, Healthy: healthy,
			AgentID: "agent", SessionID: "session",
		}); err != nil {
			t.Fatal(err)
		}
	}
	become()
	t.Cleanup(l.resign)
	assertPending()
	if err := l.BeginSession("agent", "session", []string{"a", "b"}, []string{"a", "b"}, true); err != nil {
		t.Fatal(err)
	}
	if err := l.Heartbeat("agent", "session", true); err != nil {
		t.Fatal(err)
	}
	assertPending()
	observe("a", 1, true)
	observe("b", 2, true)
	assertPending()
	observe("a", 2, false)
	product, allocations, err := l.IngressState()
	if err != nil || !l.Publishing() || product.LogIndex != 1 || allocations["a"].Healthy || !allocations["b"].Healthy {
		t.Fatalf("fresh view: product=%v allocations=%v error=%v", product, allocations, err)
	}
	l.resign()
	if _, _, err := l.IngressState(); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("former owner read = %v", err)
	}
	become()
	assertPending()
	// A durable withdrawal needs no health report from the removed workload.
	state.LogIndex++
	delete(state.Assignments, "a")
	delete(state.Assignments, "b")
	l.applyTestState(state)
	if !l.Publishing() {
		t.Fatal("durable withdrawal blocked on absent observations")
	}
}

func TestCanceledOwnershipFencesPublicationAndReports(t *testing.T) {
	l := NewLive()
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.become(ctx, func(_ context.Context, fn func(*sql.Tx, *journal.Projection) error) error {
		return fn(nil, journal.NewProjection(journal.DurableState{}))
	}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.resign)
	if !l.Publishing() {
		t.Fatal("empty cluster could not publish")
	}
	cancel()
	l.SetPublishing(true)
	if l.Publishing() {
		t.Fatal("canceled owner can publish")
	}
	if _, err := l.PublicationContext(); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("canceled ownership context = %v", err)
	}
	if err := l.BeginSession("agent", "session", nil, nil, true); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("canceled owner accepted session: %v", err)
	}
}
