package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"

	"google.golang.org/protobuf/proto"
)

func platformv1RestartClone(in *platformv1.RestartObservation) *platformv1.RestartObservation {
	if in == nil {
		return nil
	}
	out, _ := proto.Clone(in).(*platformv1.RestartObservation)
	return out
}

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

func TestBecomeDoesNotServeUntilDurableApplied(t *testing.T) {
	l := NewLive()
	started := make(chan struct{})
	release := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- l.become(context.Background(), func(_ context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
			close(started)
			<-release
			return fn(nil, journal.DurableState{
				ClusterID: "next",
				Agents:    map[string]journal.AgentRegistration{"agent": {ID: "agent", CreatedAt: time.Now().UTC()}},
			})
		}, nil)
	}()
	<-started
	if _, err := l.AgentIDsIfServing(); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("IfServing during acquire = %v, want ErrNotLiveOwner", err)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	ids, err := l.AgentIDsIfServing()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "agent" {
		t.Fatalf("ids = %v", ids)
	}
	l.resign()
	if _, err := l.AgentIDsIfServing(); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("IfServing after resign = %v", err)
	}
}

func TestBecomeAfterResignReloadsSameDurableSnapshot(t *testing.T) {
	l := NewLive()
	state := journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Agents:    map[string]journal.AgentRegistration{"agent": {ID: "agent"}},
	}
	readState := func(_ context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
		return fn(nil, state)
	}
	if err := l.become(context.Background(), readState, nil); err != nil {
		t.Fatal(err)
	}
	l.resign()
	if err := l.become(context.Background(), readState, nil); err != nil {
		t.Fatal(err)
	}
	ids, err := l.AgentIDsIfServing()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "agent" {
		t.Fatalf("ids after reacquire = %v, want [agent]", ids)
	}
}

func TestLiveDurableApplicationIsMonotonic(t *testing.T) {
	l := NewLive()
	l.ApplyDurable(journal.DurableState{ClusterID: "test", LogIndex: 2, Agents: map[string]journal.AgentRegistration{"new": {ID: "new"}}})
	l.ApplyDurable(journal.DurableState{ClusterID: "test", LogIndex: 1})
	if got := l.Durable().LogIndex; got != 2 {
		t.Fatalf("durable log index regressed to %d", got)
	}
	if _, ok := l.Durable().Agents["new"]; !ok {
		t.Fatal("stale snapshot deleted newer durable content")
	}
}

func TestLiveDurableApplicationRemembersClusterPositionAcrossSwitches(t *testing.T) {
	l := NewLive()
	l.ApplyDurable(journal.DurableState{ClusterID: "a", LogIndex: 4, Agents: map[string]journal.AgentRegistration{"new": {ID: "new"}}})
	l.ApplyDurable(journal.DurableState{ClusterID: "b", LogIndex: 1})
	l.ApplyDurable(journal.DurableState{ClusterID: "a", LogIndex: 3})
	if got := l.Durable(); got.ClusterID != "b" || got.LogIndex != 1 {
		t.Fatalf("stale prior-cluster state was reapplied: cluster=%q index=%d", got.ClusterID, got.LogIndex)
	}
}

func TestBecomeDoesNotOverwriteConcurrentNewerDurableState(t *testing.T) {
	l := NewLive()
	err := l.become(context.Background(), func(_ context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
		return fn(nil, journal.DurableState{ClusterID: "test", LogIndex: 1})
	}, func(context.Context) (uint64, error) {
		l.ApplyDurable(journal.DurableState{ClusterID: "test", LogIndex: 2, Agents: map[string]journal.AgentRegistration{"new": {ID: "new"}}})
		return 3, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Durable().LogIndex; got != 2 {
		t.Fatalf("become regressed durable log index to %d", got)
	}
	if _, ok := l.Durable().Agents["new"]; !ok {
		t.Fatal("become overwrote newer durable content")
	}
}

func TestRejectedStatusReportDoesNotConsumeSequence(t *testing.T) {
	l := startLive(t)
	now := time.Now().UTC()
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Agents:    map[string]journal.AgentRegistration{"agent": {ID: "agent", CreatedAt: now}},
		Services:  map[string]journal.ServiceIntent{"svc": {ID: "svc"}},
		Assignments: map[string]journal.Assignment{
			"alloc": {
				ID: "alloc", ServiceID: "svc", AgentID: "agent",
				DesiredRolloutGeneration: 1, DesiredSpecRevision: 1,
				AllocationIPv4: "10.0.0.2", AllocationIPv6: "fd00::2",
			},
		},
	})
	if err := l.BeginSession("agent", "s1", nil, []string{"alloc"}, true); err != nil {
		t.Fatal(err)
	}
	d := &Delivery{live: l}
	bad := &agentv1.StatusReport{
		AgentId: "agent", SessionId: "s1", ObservationSequence: 1,
		Services: []*agentv1.ServiceCondition{{
			AllocationId: "foreign", ServiceId: "svc",
			DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1,
			DesiredSpecRevision: 1, AppliedSpecRevision: 1,
		}},
	}
	if _, _, err := d.recordStatusReport(context.Background(), "agent", bad); !errors.Is(err, ErrAllocationOwnership) {
		t.Fatalf("foreign report: %v", err)
	}
	if err := l.AcceptReport("agent", "s1", 1, []string{"alloc"}, true); err != nil {
		t.Fatalf("sequence 1 should still be available after rejected report: %v", err)
	}
}

func TestStatusReportChecksLiveOwnershipBeforePayload(t *testing.T) {
	l := startLive(t)
	l.resign()
	d := &Delivery{live: l}
	if _, _, err := d.recordStatusReport(context.Background(), "agent", nil); !errors.Is(err, ErrNotLiveOwner) {
		t.Fatalf("non-owner invalid report = %v, want ErrNotLiveOwner", err)
	}
}

func TestRegisterAgentRejectsMissingWireGuardListenPortBeforeStoreAccess(t *testing.T) {
	d := &Delivery{}
	if _, err := d.RegisterAgent(context.Background(), &agentv1.AgentHello{}); err == nil {
		t.Fatal("missing wireguard listen port was accepted")
	}
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

func TestLiveAdmissionReevaluatedOnStatusReport(t *testing.T) {
	l := startLive(t)
	now := time.Now().UTC()
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Agents:    map[string]journal.AgentRegistration{"agent": {ID: "agent", Name: "agent", CreatedAt: now}},
		Administration: map[string]journal.AgentAdministration{
			"agent": {AgentID: "agent", LifecycleState: string(AgentStateActive)},
		},
		Assignments: map[string]journal.Assignment{
			"alloc": {ID: "alloc", ServiceID: "svc", AgentID: "agent", DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, RolloutState: AllocationRolloutServing},
		},
	})
	// The session begins before the agent has started its assigned allocation, so
	// it is not yet schedulable.
	if err := l.BeginSession("agent", "s1", nil, []string{"alloc"}, true); err != nil {
		t.Fatal(err)
	}
	if l.Admitted("agent") {
		t.Fatal("agent admitted before inventory reconciled")
	}
	// A later status report that includes the allocation admits the agent without
	// requiring a reconnect.
	if err := l.AcceptReport("agent", "s1", 1, []string{"alloc"}, true); err != nil {
		t.Fatal(err)
	}
	if !l.Admitted("agent") {
		t.Fatal("agent not admitted after inventory reconciled")
	}
	// Admission is sticky: an already-admitted agent is not demoted by a later
	// incomplete report, so normal rollouts do not flap scheduling.
	if err := l.AcceptReport("agent", "s1", 2, nil, true); err != nil {
		t.Fatal(err)
	}
	if !l.Admitted("agent") {
		t.Fatal("agent lost admission on a later incomplete report")
	}
}

func TestLiveSessionReplacementInvalidatesObservations(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.AcceptReport("agent", "s1", 1, nil, true); err != nil {
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
	if err := l.AcceptReport("agent", "s1", 1, nil, true); err != nil {
		t.Fatal(err)
	}
	obs := AllocationObservation{AllocationID: "alloc", RolloutGeneration: 1, Phase: "Healthy", Healthy: true, AgentID: "agent", SessionID: "s1", Sequence: 1}
	changed, err := l.RecordObservation(obs)
	if err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if err := l.AcceptReport("agent", "s1", 2, nil, true); err != nil {
		t.Fatal(err)
	}
	obs.Sequence = 2
	changed, err = l.RecordObservation(obs)
	if err != nil || changed {
		t.Fatalf("unchanged: %v %v", changed, err)
	}
}

func TestLiveObservationIgnoresMessageAndPhaseDetail(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.AcceptReport("agent", "s1", 1, nil, true); err != nil {
		t.Fatal(err)
	}
	base := AllocationObservation{AllocationID: "alloc", RolloutGeneration: 1, Phase: "Healthy", Message: "ok", Healthy: true, AgentID: "agent", SessionID: "s1", Sequence: 1}
	if changed, err := l.RecordObservation(base); err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	seq := uint64(2)
	record := func(mut func(*AllocationObservation)) bool {
		if err := l.AcceptReport("agent", "s1", seq, nil, true); err != nil {
			t.Fatal(err)
		}
		next := base
		next.Sequence = seq
		seq++
		if next.Restart != nil {
			next.Restart = platformv1RestartClone(next.Restart)
		}
		if mut != nil {
			mut(&next)
		}
		changed, err := l.RecordObservation(next)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			base = next
		}
		return changed
	}
	if record(func(o *AllocationObservation) { o.Message = "still ok but reworded" }) {
		t.Fatal("message-only change triggered work")
	}
	if record(func(o *AllocationObservation) { o.Phase = "Starting" }) {
		t.Fatal("Healthy->Starting (same class) triggered work")
	}
	if !record(func(o *AllocationObservation) { o.Phase = "Error" }) {
		t.Fatal("Healthy->Error (class change) did not trigger work")
	}
	if record(func(o *AllocationObservation) { o.Phase = "Failed"; o.Message = "different text" }) {
		t.Fatal("Error->Failed (same error class) triggered work")
	}
	base.Restart = &platformv1.RestartObservation{RestartCount: 1, Message: "flapping"}
	if record(nil) {
		t.Fatal("restart observation without crashloop triggered work")
	}
	if record(func(o *AllocationObservation) { o.Restart.RestartCount = 5; o.Restart.Message = "still flapping" }) {
		t.Fatal("restart count/message change triggered work")
	}
	if !record(func(o *AllocationObservation) { o.Restart.CrashLoop = true }) {
		t.Fatal("crashloop flag did not trigger work")
	}
}

func TestLiveObservationStoresCrashEvidenceWithoutTriggeringWork(t *testing.T) {
	l := startLive(t)
	if err := l.BeginSession("agent", "s1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := l.AcceptReport("agent", "s1", 1, nil, true); err != nil {
		t.Fatal(err)
	}
	base := AllocationObservation{
		AllocationID: "alloc", RolloutGeneration: 1, Phase: "Backoff", Healthy: false,
		AgentID: "agent", SessionID: "s1", Sequence: 1,
		Restart: &platformv1.RestartObservation{
			RestartCount: 1, LastCause: platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
			LastExitCode: 1, Message: "restarting",
		},
	}
	if changed, err := l.RecordObservation(base); err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if err := l.AcceptReport("agent", "s1", 2, nil, true); err != nil {
		t.Fatal(err)
	}
	next := base
	next.Sequence = 2
	next.Message = "reworded backoff"
	next.Restart = platformv1RestartClone(base.Restart)
	next.Restart.RestartCount = 3
	next.Restart.LastExitCode = 137
	next.Restart.LastCause = platformv1.RestartCause_RESTART_CAUSE_OOM_KILL
	next.Restart.Message = "OOM kill"
	if changed, err := l.RecordObservation(next); err != nil || changed {
		t.Fatalf("crash evidence update triggered work: %v %v", changed, err)
	}
	stored, ok := l.Observation("alloc", 1)
	if !ok {
		t.Fatal("missing stored observation")
	}
	if stored.Restart.GetRestartCount() != 3 || stored.Restart.GetLastExitCode() != 137 {
		t.Fatalf("stored crash evidence is stale: %+v", stored.Restart)
	}
	if stored.Restart.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("stored cause = %s, want OOM_KILL", stored.Restart.GetLastCause())
	}
	if stored.Message != "reworded backoff" {
		t.Fatalf("stored message = %q, want reworded backoff", stored.Message)
	}
}

func TestLiveOverlayPreservesCrashEvidenceWhenUnavailable(t *testing.T) {
	now := time.Now().UTC()
	rec := AllocationRecord{ID: "alloc", AgentID: "agent", RolloutState: AllocationRolloutServing}
	obs := AllocationObservation{
		AllocationID: "alloc", Phase: "CrashLoop", Message: "crash loop after OOM kill",
		Restart: &platformv1.RestartObservation{
			RestartCount: 5, CrashLoop: true,
			LastCause:    platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
			LastExitCode: 137, Message: "crash loop after OOM kill",
		},
		ObservedAt: now,
	}
	got := overlayAllocation(rec, AgentSession{}, false, obs, true, now, AgentHealthyTTL)
	if got.Phase != "Unavailable" {
		t.Fatalf("phase = %q, want Unavailable", got.Phase)
	}
	if got.Restart.GetRestartCount() != 5 || !got.Restart.GetCrashLoop() {
		t.Fatalf("crash evidence lost on unavailable overlay: %+v", got.Restart)
	}
	if got.Restart.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("cause = %s, want OOM_KILL", got.Restart.GetLastCause())
	}
}

func TestAdvanceRolloutSkipsTxWhenIdle(t *testing.T) {
	l := startLive(t)
	now := time.Now().UTC()
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Services: map[string]journal.ServiceIntent{
			"svc": {ID: "svc", EnvironmentID: "env", CurrentRolloutGeneration: 1},
		},
		Rollouts: map[string]journal.Rollout{
			"svc/1": {ServiceID: "svc", RolloutGeneration: 1, State: rolloutStateSucceeded},
		},
	})
	d := &Delivery{live: l}
	result, err := d.advanceRollout(context.Background(), "svc", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.EnvironmentID != "env" {
		t.Fatalf("idle advance: %+v", result)
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

func TestAllocationsByServiceTracksAppliedDurableMutations(t *testing.T) {
	l := startLive(t)
	raw := json.RawMessage(`{"runtime":{"cpuMillis":100,"memoryMebibytes":128}}`)
	now := time.Now().UTC()
	assignment := journal.Assignment{
		ID: "alloc", ServiceID: "svc", DeploymentID: "dep", AgentID: "agent",
		DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, RolloutState: AllocationRolloutServing,
		AllocationIPv4: "10.0.0.2", AllocationIPv6: "fd00::2", Intent: allocationIntentRun,
	}
	durable := func(logIndex int64, assignments map[string]journal.Assignment) journal.DurableState {
		return journal.DurableState{
			ClusterID:    "test",
			LogIndex:     logIndex,
			Projects:     map[string]journal.Project{"proj": {ID: "proj", Name: "demo"}},
			Environments: map[string]journal.Environment{"env": {ID: "env", ProjectID: "proj", Name: "prod", NetworkIdentity: 1}},
			Agents:       map[string]journal.AgentRegistration{"agent": {ID: "agent", Name: "agent", CreatedAt: now}},
			Administration: map[string]journal.AgentAdministration{
				"agent": {AgentID: "agent", LifecycleState: string(AgentStateActive)},
			},
			Services: map[string]journal.ServiceIntent{
				"svc": {ID: "svc", EnvironmentID: "env", Name: "web", CurrentSpecRevision: 1, CurrentRolloutGeneration: 1, CreatedAt: now},
			},
			Revisions:   map[string]journal.ServiceRevision{"svc/1": {ServiceID: "svc", SpecRevision: 1, SpecJSON: raw}},
			Rollouts:    map[string]journal.Rollout{"svc/1": {ServiceID: "svc", RolloutGeneration: 1, State: "in_progress"}},
			Deployments: map[string]journal.Deployment{"dep": {ID: "dep", ServiceID: "svc"}},
			Assignments: assignments,
		}
	}

	// create/release: the assignment appears.
	l.ApplyDurable(durable(1, map[string]journal.Assignment{"alloc": assignment}))
	assertLiveAllocationIDs(t, l, "svc", []string{"alloc"})

	// drain: rollout state changes in place.
	draining := assignment
	draining.RolloutState = AllocationRolloutDraining
	l.ApplyDurable(durable(2, map[string]journal.Assignment{"alloc": draining}))
	assertLiveAllocationIDs(t, l, "svc", []string{"alloc"})
	if got := l.AllocationsByService("svc"); len(got) != 1 || got[0].RolloutState != AllocationRolloutDraining {
		t.Fatalf("drain view = %+v", got)
	}

	// delete: the assignment disappears.
	l.ApplyDurable(durable(3, nil))
	assertLiveAllocationIDs(t, l, "svc", nil)
}

func assertLiveAllocationIDs(t *testing.T, l *Live, serviceID string, want []string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for _, alloc := range l.AllocationsByService(serviceID) {
		got = append(got, alloc.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("live allocations = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("live allocations = %v, want %v", got, want)
		}
	}
}

func TestAllocationsByEnvironmentUsesOneConsistentLiveView(t *testing.T) {
	l := startLive(t)
	l.ApplyDurable(journal.DurableState{
		ClusterID: "test",
		LogIndex:  1,
		Services: map[string]journal.ServiceIntent{
			"service-a": {ID: "service-a", EnvironmentID: "environment-1"},
			"service-b": {ID: "service-b", EnvironmentID: "environment-1"},
			"service-c": {ID: "service-c", EnvironmentID: "environment-2"},
		},
		Assignments: map[string]journal.Assignment{
			"allocation-a": {ID: "allocation-a", ServiceID: "service-a", AgentID: "agent-1"},
			"allocation-c": {ID: "allocation-c", ServiceID: "service-c", AgentID: "agent-2"},
		},
	})

	got, err := l.AllocationsByEnvironmentIfServing("environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got["service-a"]) != 1 || got["service-a"][0].ID != "allocation-a" {
		t.Fatalf("environment allocation view = %#v", got)
	}
	if allocations, ok := got["service-b"]; !ok || len(allocations) != 0 {
		t.Fatalf("service without allocations missing from view: %#v", got)
	}
	if _, ok := got["service-c"]; ok {
		t.Fatalf("allocation from another environment leaked into view: %#v", got)
	}
}
