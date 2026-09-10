//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"testing"
	"time"
)

func TestRolloutAdvancementRollbackAndConcurrency(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	target := allocationForGeneration(t, store, service.ID, 2)[0]
	markRolloutAllocationReady(t, store, target)
	now := time.Now().UTC()
	revision := mustDesiredRevision(t, store, ctx, target.AgentID)
	aborted := errors.New("abort after rollout writes")
	deps := deliveryDependencies(store, nil, nil, nil, nil)
	deps.Transaction = func(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
		return store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return aborted
		})
	}
	engine := deliverycore.New(deps)
	engine.SetClocks(func() time.Time { return now }, nil)
	err := engine.ReconcileRollouts(ctx)
	if !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	if got := allocationByID(t, store, service.ID, old.ID); got.RolloutState != deliverycore.AllocationRolloutServing {
		t.Fatalf("withdrawal escaped rollback: %+v", got)
	}
	if got := allocationByID(t, store, service.ID, target.ID); got.RolloutState != deliverycore.AllocationRolloutStarting {
		t.Fatalf("promotion escaped rollback: %+v", got)
	}
	if got := mustDesiredRevision(t, store, ctx, target.AgentID); got != revision {
		t.Fatalf("revision escaped rollback: %d -> %d", revision, got)
	}

	// Competing transactions must plan from the locked state. The loser observes
	// the durable withdrawal and cannot promote or bump the revision again.
	waiting := errors.New("ingress has not converged")
	probe := &blockedRolloutIngress{err: waiting}
	engine = deliverycore.New(deliveryDependencies(store, nil, probe, nil, nil))
	engine.SetClocks(func() time.Time { return now }, nil)
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; outcomes <- engine.ReconcileRollouts(ctx) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-outcomes; !errors.Is(err, waiting) {
			t.Fatalf("expected durable withdrawal awaiting ingress: %v", err)
		}
	}
	if got := mustDesiredRevision(t, store, ctx, target.AgentID); got != revision+1 {
		t.Fatalf("revision = %d, want %d", got, revision+1)
	}
	if got := allocationByID(t, store, service.ID, old.ID); got.RolloutState != deliverycore.AllocationRolloutWithdrawing || got.DrainDeadline.Valid {
		t.Fatalf("drained without ingress confirmation: %+v", got)
	}
}

type blockedRolloutIngress struct{ err error }

func (p *blockedRolloutIngress) RequestSync()               {}
func (p *blockedRolloutIngress) Sync(context.Context) error { return p.err }
