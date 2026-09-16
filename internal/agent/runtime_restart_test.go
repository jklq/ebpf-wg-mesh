package agent

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReconcileDoesNotRecreateWhenPolicyIsNever(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 0}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	runtime := newRestartTestRuntime(t, dir, engine, nil)
	report, err := runtime.Reconcile(context.Background(), neverRestartState())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := report.GetServices()[0].GetPhase(); got != restartpolicy.PhaseStopped {
		t.Fatalf("phase = %q, want Stopped", got)
	}
	if len(engine.removed) != 0 {
		t.Fatalf("never policy recreated the container: removed=%v ensured=%v", engine.removed, engine.ensured)
	}
}

func TestReconcileStopsAfterRetryBudgetAndDoesNotSpin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 2}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       1,
		WindowSeconds:     60,
		InitialDelayMs:    0,
		MaxDelayMs:        0,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	first, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if first.GetServices()[0].GetPhase() == restartpolicy.PhaseCrashLoop {
		t.Fatal("first failure should restart, not crash-loop")
	}
	engine.stopped["alloc-1"] = true
	engine.created["alloc-1"] = false
	second, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if second.GetServices()[0].GetPhase() != restartpolicy.PhaseCrashLoop {
		t.Fatalf("phase = %q, want CrashLoop", second.GetServices()[0].GetPhase())
	}
	if second.GetServices()[0].GetHealthy() {
		t.Fatal("crash-loop allocation must not be healthy")
	}
	removedAfterLoop := len(engine.removed)
	for i := 0; i < 5; i++ {
		report, err := runtime.Reconcile(context.Background(), state)
		if err != nil {
			t.Fatalf("spin Reconcile: %v", err)
		}
		if report.GetServices()[0].GetPhase() != restartpolicy.PhaseCrashLoop {
			t.Fatalf("reconcile left crash-loop: %s", report.GetServices()[0].GetPhase())
		}
	}
	if len(engine.removed) != removedAfterLoop {
		t.Fatalf("reconcile recreated containers after crash-loop: removed %d -> %d", removedAfterLoop, len(engine.removed))
	}
}

func TestReconcileRestoresObservationAfterAgentRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 2}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       1,
		InitialDelayMs:    0,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("seed Reconcile: %v", err)
	}
	engine.stopped["alloc-1"] = true
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("crash-loop Reconcile: %v", err)
	}
	restored := newRestartTestRuntime(t, dir, engine, clock)
	report, err := restored.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("restored Reconcile: %v", err)
	}
	if report.GetServices()[0].GetPhase() != restartpolicy.PhaseCrashLoop {
		t.Fatalf("local observation did not survive runtime rebuild: %s", report.GetServices()[0].GetPhase())
	}
}

func TestReconcileUsesControlPlaneObservationWhenLocalHistoryIsMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 2}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       1,
		InitialDelayMs:    0,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	state.Services[0].RestartObservation = &platformv1.RestartObservation{
		RestartCount:             1,
		CrashLoop:                true,
		AppliedRolloutGeneration: 1,
		LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
	}
	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.GetServices()[0].GetPhase() != restartpolicy.PhaseCrashLoop {
		t.Fatalf("control-plane observation was ignored: %s", report.GetServices()[0].GetPhase())
	}
}

func TestReconcileAuthorizedNonceRestartsAfterCrashLoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 2}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       1,
		InitialDelayMs:    0,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	state.Services[0].RestartObservation = &platformv1.RestartObservation{
		RestartCount:             1,
		CrashLoop:                true,
		AppliedRolloutGeneration: 1,
	}
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("crash-loop Reconcile: %v", err)
	}
	ensured := len(engine.ensured)
	state.Services[0].OperatorRestartNonce = 1
	engine.created["alloc-1"] = true
	engine.stopped["alloc-1"] = false
	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("operator Reconcile: %v", err)
	}
	if report.GetServices()[0].GetPhase() == restartpolicy.PhaseCrashLoop {
		t.Fatal("operator restart left the allocation in crash-loop")
	}
	if len(engine.ensured) <= ensured {
		t.Fatal("operator restart did not start the process")
	}
}

func TestReconcileBackoffHonorsInjectedClock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 2}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       3,
		InitialDelayMs:    5000,
		MaxDelayMs:        5000,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	first, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if first.GetServices()[0].GetPhase() != restartpolicy.PhaseBackoff {
		t.Fatalf("phase = %q, want Backoff", first.GetServices()[0].GetPhase())
	}
	removed := len(engine.removed)
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(engine.removed) != removed {
		t.Fatal("backoff window recreated the container")
	}
	clock.Advance(5 * time.Second)
	engine.created["alloc-1"] = true
	engine.stopped["alloc-1"] = false
	after, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("after clock advance: %v", err)
	}
	if after.GetServices()[0].GetPhase() == restartpolicy.PhaseBackoff {
		t.Fatal("clock advance did not release backoff")
	}
}

func newRestartTestRuntime(t *testing.T, dir string, engine *fakeEngine, clock restartpolicy.Clock) *ContainerdRuntime {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if clock == nil {
		clock = restartpolicy.SystemClock{}
	}
	return &ContainerdRuntime{
		cfg: config.AgentConfig{Runtime: config.RuntimeConfig{
			DataDir:    dir,
			VolumesDir: filepath.Join(dir, "volumes"),
		}},
		engine: engine,
		clock:  clock,
		rng:    rand.New(rand.NewSource(1)),
	}
}

func TestReconcilePreservesCrashLoopAfterContainerRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AppliedSpecRevision: 1, AppliedRolloutGeneration: 1, ExitCode: 137}},
		created: map[string]bool{"alloc-1": false},
		stopped: map[string]bool{"alloc-1": true},
	}
	clock := restartpolicy.NewManualClock(time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC))
	runtime := newRestartTestRuntime(t, dir, engine, clock)
	state := restartState(&platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       1,
		InitialDelayMs:    0,
		BackoffMultiplier: 1,
		Jitter:            0,
	})
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("seed Reconcile: %v", err)
	}
	engine.stopped["alloc-1"] = true
	looped, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("crash-loop Reconcile: %v", err)
	}
	before := looped.GetServices()[0].GetRestart()
	if looped.GetServices()[0].GetPhase() != restartpolicy.PhaseCrashLoop {
		t.Fatalf("phase = %q, want CrashLoop", looped.GetServices()[0].GetPhase())
	}
	engine.created["alloc-1"] = true
	engine.stopped["alloc-1"] = false
	engine.status["alloc-1"] = serviceStatus{AppliedSpecRevision: 1, AppliedRolloutGeneration: 1}
	ensuredBefore := len(engine.ensured)
	after, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("removed-container Reconcile: %v", err)
	}
	if len(engine.ensured) != ensuredBefore {
		t.Fatalf("preserved crash-loop started a container: ensured=%v inspected=%v", engine.ensured, engine.inspected)
	}
	cond := after.GetServices()[0]
	if cond.GetPhase() != restartpolicy.PhaseCrashLoop {
		t.Fatalf("container removal cleared crash-loop: phase = %q", cond.GetPhase())
	}
	if cond.GetHealthy() {
		t.Fatal("crash-loop allocation must not be healthy after container removal")
	}
	got := cond.GetRestart()
	if got.GetRestartCount() != before.GetRestartCount() || got.GetLastExitCode() != 137 {
		t.Fatalf("crash evidence lost after removal: before=%+v after=%+v", before, got)
	}
	if got.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO {
		t.Fatalf("last cause = %s, want EXIT_NONZERO", got.GetLastCause())
	}
}

func TestReconcilePreservedTerminalNeverStartsMissingContainer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC)
	onFailure := &platformv1.ServiceRestart{Policy: platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE}
	cases := []struct {
		name        string
		restart     *platformv1.ServiceRestart
		observation *platformv1.RestartObservation
		wantPhase   string
	}{
		{
			name:    "crash loop",
			restart: onFailure,
			observation: &platformv1.RestartObservation{
				RestartCount: 5, CrashLoop: true,
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
				LastExitCode:             137,
				AppliedRolloutGeneration: 1,
			},
			wantPhase: restartpolicy.PhaseCrashLoop,
		},
		{
			name:    "backoff",
			restart: onFailure,
			observation: &platformv1.RestartObservation{
				RestartCount: 1, AwaitingRestart: true,
				NextRestartAt:            timestamppb.New(now.Add(30 * time.Second)),
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
				LastExitCode:             2,
				AppliedRolloutGeneration: 1,
			},
			wantPhase: restartpolicy.PhaseBackoff,
		},
		{
			name:    "terminal stop",
			restart: &platformv1.ServiceRestart{Policy: platformv1.RestartPolicy_RESTART_POLICY_NEVER},
			observation: &platformv1.RestartObservation{
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
				LastExitCode:             3,
				AppliedRolloutGeneration: 1,
			},
			wantPhase: restartpolicy.PhaseStopped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			engine := &fakeEngine{missing: map[string]bool{"alloc-1": true}}
			runtime := newRestartTestRuntime(t, dir, engine, restartpolicy.NewManualClock(now))
			state := restartState(tc.restart)
			state.Services[0].RestartObservation = tc.observation
			var cond *agentv1.ServiceCondition
			// Repeated reconciles simulate safety resyncs: none may start the
			// missing container.
			for i := 0; i < 3; i++ {
				report, err := runtime.Reconcile(context.Background(), state)
				if err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
				cond = report.GetServices()[0]
				if cond.GetPhase() != tc.wantPhase {
					t.Fatalf("reconcile %d: phase = %q, want %q", i, cond.GetPhase(), tc.wantPhase)
				}
				if cond.GetHealthy() {
					t.Fatalf("reconcile %d: terminal allocation must not be healthy", i)
				}
			}
			if len(engine.ensured) != 0 {
				t.Fatalf("terminal allocation started a missing container: ensured=%v", engine.ensured)
			}
			if len(engine.inspected) != 3 {
				t.Fatalf("saved state was not checked before deciding: inspected=%v", engine.inspected)
			}
			got := cond.GetRestart()
			want := tc.observation
			if got.GetRestartCount() != want.GetRestartCount() ||
				got.GetLastExitCode() != want.GetLastExitCode() ||
				got.GetLastCause() != want.GetLastCause() ||
				got.GetCrashLoop() != want.GetCrashLoop() ||
				got.GetAwaitingRestart() != want.GetAwaitingRestart() {
				t.Fatalf("crash evidence not preserved: got=%+v want=%+v", got, want)
			}
		})
	}
}

func neverRestartState() *agentv1.DesiredNodeState {
	return restartState(&platformv1.ServiceRestart{Policy: platformv1.RestartPolicy_RESTART_POLICY_NEVER})
}

func restartState(restart *platformv1.ServiceRestart) *agentv1.DesiredNodeState {
	return &agentv1.DesiredNodeState{
		AgentId: "node-1",
		Services: []*agentv1.DesiredService{{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			DesiredSpecRevision:      1,
			DesiredRolloutGeneration: 1,
			RestartObservation: &platformv1.RestartObservation{
				AppliedRolloutGeneration: 1,
			},
			Spec: &platformv1.ResolvedServiceSpec{
				Image: "example.com/test@sha256:abc",
				Runtime: &platformv1.ServiceRuntime{
					Restart: restart,
				},
			},
		}},
	}
}
