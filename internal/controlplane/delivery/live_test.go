package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func startLive(t *testing.T) *Live {
	t.Helper()
	l := NewLive()
	if err := l.become(context.Background(), func(_ context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
		return fn(nil, journal.DurableState{ClusterID: "test"})
	}, nil); err != nil {
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

func TestLiveWatchCoalescesAndDoesNotBlockApply(t *testing.T) {
	l := startLive(t)
	ch, stop := l.Watch("agent")
	defer stop()
	done := make(chan struct{})
	go func() {
		l.Notify("agent")
		l.Notify("agent")
		l.Notify("agent")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow consumer blocked apply")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("missing coalesced wake")
	}
	select {
	case <-ch:
		t.Fatal("expected a single coalesced notification")
	default:
	}
}

func TestLiveReadAndSubscribeSeesApply(t *testing.T) {
	l := startLive(t)
	ch, stop := l.Watch("agent")
	defer stop()
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Agents: map[string]journal.AgentRegistration{
			"agent": {ID: "agent", Name: "agent", DesiredRevision: 3, CreatedAt: time.Now().UTC()},
		},
		Administration: map[string]journal.AgentAdministration{
			"agent": {AgentID: "agent", LifecycleState: string(AgentStateActive)},
		},
	})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscribe missed apply")
	}
	rev, ok := l.DesiredRevision("agent")
	if !ok || rev != 3 {
		t.Fatalf("desired revision = %d %v", rev, ok)
	}
	pos := l.Position()
	if pos.AcceptedDurable != 1 || pos.AppliedLive == 0 || !pos.Ready {
		t.Fatalf("position %+v", pos)
	}
}

func TestLiveDesiredStateAndPlacementUseMemory(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"runtime":{"cpuMillis":100,"memoryMebibytes":128}}`)
	now := time.Now().UTC()
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  4,
		Projects:  map[string]journal.Project{"proj": {ID: "proj", Name: "demo"}},
		Environments: map[string]journal.Environment{
			"env": {ID: "env", ProjectID: "proj", Name: "prod", NetworkIdentity: 7},
		},
		Agents: map[string]journal.AgentRegistration{
			"agent": {
				ID: "agent", Name: "agent", DesiredRevision: 9,
				Region: "r1", FailureDomain: "fd1",
				CPUMillisCapacity: 1000, MemoryMebibytesCapacity: 2048,
				RuntimeCapabilities: json.RawMessage(`["containerd","wireguard","ebpf-policy"]`),
				CreatedAt:           now,
			},
		},
		Administration: map[string]journal.AgentAdministration{
			"agent": {AgentID: "agent", LifecycleState: string(AgentStateActive)},
		},
		Services: map[string]journal.ServiceIntent{
			"svc": {ID: "svc", EnvironmentID: "env", Name: "web", CurrentSpecRevision: 1, CurrentRolloutGeneration: 1, CreatedAt: now},
		},
		Revisions: map[string]journal.ServiceRevision{
			"svc/1": {ServiceID: "svc", SpecRevision: 1, SpecJSON: raw},
		},
		Rollouts: map[string]journal.Rollout{
			"svc/1": {ServiceID: "svc", RolloutGeneration: 1, ImageDigest: "sha256:abc", State: "succeeded"},
		},
		Deployments: map[string]journal.Deployment{
			"dep": {ID: "dep", ServiceID: "svc"},
		},
		Assignments: map[string]journal.Assignment{
			"alloc": {
				ID: "alloc", ServiceID: "svc", DeploymentID: "dep", AgentID: "agent",
				DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, RolloutState: AllocationRolloutServing,
				AllocationIPv4: "10.0.0.2", AllocationIPv6: "fd00::2", Intent: allocationIntentRun,
			},
		},
		Domains: map[string]journal.Domain{
			"web.example": {Hostname: "web.example", ServiceID: "svc", TargetPort: 8080},
		},
	})
	state, err := l.DesiredStateForAgent("agent", config.ControlPlaneMeshConfig{WorkloadIPv4PoolCIDR: "10.200.0.0/16", WorkloadPoolCIDR: "fd00::/64"})
	if err != nil {
		t.Fatal(err)
	}
	if state.GetReconciliationCursor() != 9 || len(state.GetServices()) != 1 {
		t.Fatalf("desired %+v", state)
	}
	if got := state.GetServices()[0].GetSpec().GetRuntime().GetPorts(); len(got) == 0 || got[0].GetPort() != 8080 {
		t.Fatalf("domain ports = %+v", got)
	}
	candidates := l.PlacementCandidates()
	if len(candidates) != 1 || candidates[0].ID != "agent" || candidates[0].ServiceCount != 1 {
		t.Fatalf("placement %+v", candidates)
	}
	allocs := l.AllocationsByService("svc")
	if len(allocs) != 1 || allocs[0].ID != "alloc" {
		t.Fatalf("allocations %+v", allocs)
	}
	if ids := l.InProgressRolloutServiceIDs(); len(ids) != 0 {
		t.Fatalf("in progress %v", ids)
	}
}
