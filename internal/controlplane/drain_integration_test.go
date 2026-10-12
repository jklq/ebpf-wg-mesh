//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

// A draining live owner hands its lease to a peer before its agents reconnect,
// so they follow the successor instead of waiting out the old owner's drain.
func TestDrainingOwnerHandsAgentsToItsSuccessor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := startSystemControlPlane(t, systemControlPlaneOptions{})
	standby := startSystemControlPlane(t, systemControlPlaneOptions{
		databaseURL: owner.cfg.Database.URL,
		stateDir:    owner.cfg.StateDir,
		standby:     true,
	})
	cert := enrollAgentTLS(t, owner.server, "system-test-agent", "system-test-bootstrap")
	stream, closeStream := openAgentSync(t, owner.server, cert, agentHello("system-test-agent"))
	defer closeStream()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("owner session: %v", err)
	}

	// SIGTERM cancels Run's context; Close is deliberately not called here.
	drainStarted := time.Now()
	owner.cancel()
	var err error
	for err == nil {
		_, err = stream.Recv()
	}
	if ended := time.Since(drainStarted); ended > 5*time.Second {
		t.Fatalf("agent session lingered %v on the draining owner", ended)
	}
	// Usually the released lease ends the session with ownership guidance; a
	// session busy planning when the lease moves is shed instead. Both tell the
	// agent where to go next, and neither quarantines the successor.
	reason, after, ok := agentv1.ReconnectGuidance(err)
	if !ok || reason == agentv1.ReasonOwnerNotReady && after <= 0 {
		t.Fatalf("agent session ended with %v, want reconnect guidance", err)
	}

	for {
		held, _, lookupErr := standby.server.leases.Lookup(ctx, singletonLeaseName)
		if lookupErr == nil && held {
			break
		}
		if ctx.Err() != nil || time.Since(drainStarted) > 10*time.Second {
			t.Fatalf("successor did not acquire the released lease: %v", lookupErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	resumed, closeResumed := openAgentSync(t, standby.server, cert, agentHello("system-test-agent"))
	defer closeResumed()
	if _, err := resumed.Recv(); err != nil {
		t.Fatalf("successor refused the agent: %v", err)
	}
	handover := time.Since(drainStarted)
	if handover > 10*time.Second {
		t.Fatalf("agent resumed %v after the owner began draining", handover)
	}
	t.Logf("session ended with %s after %v; agent resumed on the successor after %v", reason, after, handover)
}
