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
	after, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("removed-container Reconcile: %v", err)
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
