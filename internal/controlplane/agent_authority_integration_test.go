//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
)

func TestAuthorityCutoverWaitsForPartitionedAgentGrant(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := &agentv1.AgentHello{AgentId: "isolated", SessionId: "former"}
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	epoch, err := store.agentAuthorityEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, epoch, 0)
	if err != nil {
		t.Fatal(err)
	}
	delayed := &agentv1.DesiredNodeState{}
	stampAgentCommand(delayed, hello.SessionId, epoch, deadline)
	// Simulate a holder losing its stream while an already granted command
	// remains buffered in the network. Disconnect must not shorten the grant.
	if err := testDelivery(store).EndAgentSession(ctx, hello.AgentId, hello.SessionId); err != nil {
		t.Fatal(err)
	}
	if err := store.advanceAgentAuthority(ctx, epoch); err == nil {
		t.Fatal("takeover bypassed outstanding grant")
	}
	// Exercise the actual persisted wall-clock deadline, not a synthetic epoch
	// update. The agent stays partitioned and never observes the successor.
	time.Sleep(time.Until(deadline) + 10*time.Millisecond)
	if err := store.advanceAgentAuthority(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if err := reconciliation.ValidateCommand(delayed, hello.SessionId, time.Now().Add(-reconciliation.MaxClockSkew)); err == nil {
		t.Fatal("isolated agent accepted delayed former command")
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, epoch, 1); err == nil {
		t.Fatal("paused former holder renewed after takeover")
	}
	if err := store.advanceAgentAuthority(ctx, epoch); err == nil {
		t.Fatal("stale owner advanced epoch twice")
	}
}

func TestSessionAcknowledgementAndFreshStoreRecovery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := &agentv1.AgentHello{AgentId: "agent", SessionId: "first", LocalStoreId: "durable-store"}
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, 1, 7); err != nil {
		t.Fatal(err)
	}
	ack := &agentv1.DesiredStateAcknowledgement{AgentId: hello.AgentId, SessionId: hello.SessionId, AuthorityEpoch: 1, ReconciliationCursor: 7}
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err != nil {
		t.Fatal(err)
	}
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err != nil {
		t.Fatalf("duplicate durable acceptance: %v", err)
	}
	session, ok := fixtureLive(store).Session(hello.AgentId)
	if !ok {
		t.Fatal("missing live session")
	}
	if session.Sequence != 0 || session.AcceptedCursor != 7 {
		t.Fatalf("ack was interpreted as runtime observation: sequence=%d cursor=%d", session.Sequence, session.AcceptedCursor)
	}
	ack.ReconciliationCursor = 8
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err == nil {
		t.Fatal("accepted acknowledgement beyond offered state")
	}
	// Empty storage cannot take over an existing identity, even with a valid
	// enrollment credential and no discoverable runtime inventory.
	replacement := &agentv1.AgentHello{AgentId: hello.AgentId, SessionId: "empty", LocalStoreId: "fresh-store", InitializationState: "uninitialized"}
	if _, err := testDelivery(store).RegisterAgent(ctx, replacement); err == nil {
		t.Fatal("fresh storage reused existing identity")
	}
	hello.SessionId = "second"
	hello.SessionIncarnation++
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	hello.SessionIncarnation--
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err == nil {
		t.Fatal("delayed hello superseded a newer incarnation")
	}
	ack.ReconciliationCursor = 7
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err == nil {
		t.Fatal("accepted delayed acknowledgement from superseded session")
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, "first", 1, 8); err == nil {
		t.Fatal("superseded session received new command grant")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.AgentId, "first", false); err == nil {
		t.Fatal("superseded heartbeat accepted")
	}
}
