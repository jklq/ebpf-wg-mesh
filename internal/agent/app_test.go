package agent

import (
	"context"
	"errors"
	"sync"
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

func TestPeriodicReconcileLoopUsesLatestDesiredState(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu          sync.RWMutex
		latest      *agentv1.DesiredNodeState
		revisionsMu sync.Mutex
		revisions   []int64
		firstTick   = make(chan struct{}, 1)
		secondTick  = make(chan struct{}, 1)
	)

	mu.Lock()
	latest = &agentv1.DesiredNodeState{Revision: 1}
	mu.Unlock()

	go periodicReconcileLoop(ctx, 10*time.Millisecond, func() *agentv1.DesiredNodeState {
		mu.RLock()
		defer mu.RUnlock()
		if latest == nil {
			return nil
		}
		return latest
	}, func(state *agentv1.DesiredNodeState) {
		revisionsMu.Lock()
		revisions = append(revisions, state.GetRevision())
		count := len(revisions)
		revisionsMu.Unlock()
		switch count {
		case 1:
			select {
			case firstTick <- struct{}{}:
			default:
			}
		case 2:
			select {
			case secondTick <- struct{}{}:
			default:
			}
		}
	})

	select {
	case <-firstTick:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for first periodic reconcile")
	}

	mu.Lock()
	latest = &agentv1.DesiredNodeState{Revision: 2}
	mu.Unlock()

	select {
	case <-secondTick:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for second periodic reconcile")
	}

	revisionsMu.Lock()
	seen := append([]int64(nil), revisions...)
	revisionsMu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected at least two periodic reconciles, got %v", seen)
	}
	if seen[0] != 1 {
		t.Fatalf("expected first reconcile revision 1, got %v", seen)
	}
	if seen[1] != 2 {
		t.Fatalf("expected second reconcile to use latest revision 2, got %v", seen)
	}
}

func TestDesiredWorkloadsEqualIgnoresEnvelopeChanges(t *testing.T) {
	t.Parallel()

	service := &agentv1.DesiredService{AllocationId: "alloc-1", DesiredSpecRevision: 1}
	previous := &agentv1.DesiredNodeState{
		AgentId:  "node-1",
		Revision: 1,
		Services: []*agentv1.DesiredService{service},
		NodeConfig: &agentv1.AssignedNodeConfig{
			WireguardAddresses: []string{"fd00::1/128"},
		},
	}
	next := &agentv1.DesiredNodeState{
		AgentId:  "node-1",
		Revision: 2,
		Services: []*agentv1.DesiredService{service},
		NodeConfig: &agentv1.AssignedNodeConfig{
			WireguardAddresses: []string{"fd00::2/128"},
		},
	}
	if !desiredWorkloadsEqual(previous, next) {
		t.Fatal("revision and node-config-only change should not reconcile workloads")
	}

	next.Services = []*agentv1.DesiredService{{AllocationId: "alloc-1", DesiredSpecRevision: 2}}
	if desiredWorkloadsEqual(previous, next) {
		t.Fatal("service desired-state change should reconcile workloads")
	}
}

func TestStatusReportChanged(t *testing.T) {
	t.Parallel()

	previous := &agentv1.StatusReport{
		AgentId:  "node-1",
		Services: []*agentv1.ServiceCondition{{AllocationId: "alloc-1", Phase: "Healthy"}},
	}
	unchanged := &agentv1.StatusReport{
		AgentId:  "node-1",
		Services: []*agentv1.ServiceCondition{{AllocationId: "alloc-1", Phase: "Healthy"}},
	}
	if statusReportChanged(previous, unchanged) {
		t.Fatal("equivalent report should not be sent")
	}
	unchanged.Services[0].Healthy = true
	if !statusReportChanged(previous, unchanged) {
		t.Fatal("changed report should be sent")
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
