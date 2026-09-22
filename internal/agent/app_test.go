package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/mesh"
)

type recordingMesh struct {
	updates []config.MeshRuntimeConfig
	closed  int
	err     error
}

func (m *recordingMesh) Update(cfg config.MeshRuntimeConfig) error {
	m.updates = append(m.updates, cfg)
	return m.err
}

func (m *recordingMesh) Close() error {
	m.closed++
	return nil
}

func TestApplyNodeConfigUpdatesRunningMeshInPlace(t *testing.T) {
	t.Parallel()

	handle := &recordingMesh{}
	factoryCalls := 0
	app := &App{
		cfg: config.AgentConfig{Mesh: config.MeshConfig{WireGuard: config.WireGuard{
			InterfaceName: "wg0",
			PrivateKey:    "private-key",
		}}},
		meshFactory: func(context.Context, config.MeshRuntimeConfig) (MeshHandle, error) {
			factoryCalls++
			return handle, nil
		},
	}
	initial := &agentv1.AssignedNodeConfig{
		WorkloadIpv6Subnet: "fd00:44:1::/80",
		WireguardAddresses: []string{"fd00:44::1/128"},
	}
	if err := app.applyNodeConfig(context.Background(), initial); err != nil {
		t.Fatalf("initial applyNodeConfig: %v", err)
	}
	updated := &agentv1.AssignedNodeConfig{
		WorkloadIpv6Subnet: "fd00:44:1::/80",
		WireguardAddresses: []string{"fd00:44::1/128"},
		Peers: []*agentv1.WireGuardPeer{{
			Name:       "node-b",
			PublicKey:  "peer-public-key",
			Endpoint:   "[2001:db8::2]:51820",
			AllowedIps: []string{"fd00:44:2::/80"},
		}},
	}
	if err := app.applyNodeConfig(context.Background(), updated); err != nil {
		t.Fatalf("updated applyNodeConfig: %v", err)
	}

	if factoryCalls != 1 {
		t.Fatalf("expected one mesh creation, got %d", factoryCalls)
	}
	if handle.closed != 0 {
		t.Fatalf("mesh was closed during update %d times", handle.closed)
	}
	if len(handle.updates) != 1 || len(handle.updates[0].WireGuard.Peers) != 1 {
		t.Fatalf("expected updated peer assignment, got %+v", handle.updates)
	}
	if err := app.applyNodeConfig(context.Background(), updated); err != nil {
		t.Fatalf("unchanged applyNodeConfig: %v", err)
	}
	if len(handle.updates) != 1 {
		t.Fatalf("unchanged assignment triggered update: %d calls", len(handle.updates))
	}
}

func TestApplyNodeConfigKeepsPreviousAssignmentWhenUpdateFails(t *testing.T) {
	t.Parallel()

	updateErr := errors.New("update failed")
	handle := &recordingMesh{err: updateErr}
	previous := &agentv1.AssignedNodeConfig{WireguardAddresses: []string{"fd00:44::1/128"}}
	app := &App{
		cfg:            config.AgentConfig{},
		mesh:           handle,
		meshAssignment: mesh.AssignmentFrom(previous),
	}
	next := &agentv1.AssignedNodeConfig{WireguardAddresses: []string{"fd00:44::2/128"}}
	if err := app.applyNodeConfig(context.Background(), next); !errors.Is(err, updateErr) {
		t.Fatalf("applyNodeConfig error = %v, want %v", err, updateErr)
	}
	if got := app.meshAssignment.WireGuardAddresses; len(got) != 1 || got[0] != "fd00:44::1/128" {
		t.Fatalf("failed update changed assignment: %+v", app.meshAssignment)
	}
	if handle.closed != 0 {
		t.Fatalf("failed update closed mesh %d times", handle.closed)
	}
}

func TestCumulativeAckFencesToConfirmedSessionEpoch(t *testing.T) {
	t.Parallel()

	// Fresh agent: pull credentials arrive before the first checkpoint, so
	// the store's accepted allocation epoch is still zero while the session
	// runs under the confirmed authority epoch. The cumulative ack must echo
	// the confirmed epoch or the control plane rejects it as outside the
	// session authority and initial sync cannot complete.
	summary := localStateSummary{ReconciliationCursor: 0, CredentialsVersion: "creds-v1"}
	ack := cumulativeAck("node-1", "session-1", summary, 7)
	if ack.GetAuthorityEpoch() != 7 {
		t.Fatalf("ack authority epoch = %d, want confirmed session epoch 7", ack.GetAuthorityEpoch())
	}
	if ack.GetReconciliationCursor() != 0 || ack.GetCredentialsVersion() != "creds-v1" {
		t.Fatalf("ack lost cumulative position: %+v", ack)
	}
	if ack.GetAgentId() != "node-1" || ack.GetSessionId() != "session-1" {
		t.Fatalf("ack lost session identity: %+v", ack)
	}
}

func TestNextReconnectDelayBoundsReconnectStorms(t *testing.T) {
	t.Parallel()

	// A short-lived session grows the backoff to bound reconnect storms.
	if got := nextReconnectDelay(initialReconnectDelay); got != 2*time.Second {
		t.Fatalf("first unstable delay = %s, want 2s", got)
	}
	if got := nextReconnectDelay(maxReconnectDelay); got != maxReconnectDelay {
		t.Fatalf("unstable delay at maximum = %s, want %s", got, maxReconnectDelay)
	}
}

func TestRuntimeEventReconcileLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan struct{}, 1)
	errs := make(chan error, 1)
	reconciled := make(chan struct{}, 1)
	reportedErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runtimeEventReconcileLoop(ctx, events, errs, func() {
			reconciled <- struct{}{}
		}, func(err error) {
			reportedErr <- err
		})
		close(done)
	}()

	events <- struct{}{}
	select {
	case <-reconciled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime event did not trigger reconciliation")
	}
	watchErr := errors.New("watch failed")
	errs <- watchErr
	select {
	case got := <-reportedErr:
		if !errors.Is(got, watchErr) {
			t.Fatalf("reported error = %v, want %v", got, watchErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime watch error was not reported")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime event loop did not stop")
	}
}
