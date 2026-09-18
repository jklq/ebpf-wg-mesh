package restartpolicy

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestEvaluateAlwaysRestartsExitZero(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	rng := rand.New(rand.NewSource(1))
	decision := Evaluate(clock.Now(), rng, &platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ALWAYS,
		MaxRestarts:       3,
		InitialDelayMs:    0,
		MaxDelayMs:        0,
		BackoffMultiplier: 1,
		Jitter:            0,
	}, appliedObs(1), Input{State: ProcessState{ExitCode: 0}, DesiredRolloutGeneration: 1})
	if decision.Action != ActionStart {
		t.Fatalf("action = %s, want start", decision.Action)
	}
	if decision.Observation.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_EXIT_ZERO {
		t.Fatalf("cause = %s", decision.Observation.GetLastCause())
	}
}

func TestEvaluateOnFailureDoesNotRestartExitZero(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	decision := Evaluate(clock.Now(), nil, &platformv1.ServiceRestart{
		Policy:      platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts: 3,
	}, appliedObs(1), Input{State: ProcessState{ExitCode: 0}, DesiredRolloutGeneration: 1})
	if decision.Action != ActionStop || decision.Phase != PhaseStopped {
		t.Fatalf("decision = %+v, want stop", decision)
	}
}

func TestEvaluateNeverDoesNotRestartFailures(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	for _, state := range []ProcessState{
		{ExitCode: 2},
		{Signal: 9},
		{OOMKilled: true, Signal: 9},
		{DiskExhausted: true},
	} {
		decision := Evaluate(clock.Now(), nil, &platformv1.ServiceRestart{
			Policy: platformv1.RestartPolicy_RESTART_POLICY_NEVER,
		}, appliedObs(1), Input{State: state, DesiredRolloutGeneration: 1})
		if decision.Action != ActionStop {
			t.Fatalf("state %+v action = %s, want stop", state, decision.Action)
		}
	}
}

func TestEvaluateClassifiesCauses(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		name  string
		in    Input
		cause platformv1.RestartCause
	}{
		{"exit-zero", Input{State: ProcessState{ExitCode: 0}}, platformv1.RestartCause_RESTART_CAUSE_EXIT_ZERO},
		{"exit-nonzero", Input{State: ProcessState{ExitCode: 7}}, platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO},
		{"signal", Input{State: ProcessState{Signal: 15, ExitCode: 143}}, platformv1.RestartCause_RESTART_CAUSE_SIGNAL},
		{"oom", Input{State: ProcessState{OOMKilled: true, Signal: 9, ExitCode: 137}}, platformv1.RestartCause_RESTART_CAUSE_OOM_KILL},
		{"disk", Input{State: ProcessState{DiskExhausted: true}}, platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED},
		{"liveness", Input{State: ProcessState{Running: true}, LivenessFailed: true}, platformv1.RestartCause_RESTART_CAUSE_LIVENESS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.DesiredRolloutGeneration = 1
			decision := Evaluate(clock.Now(), nil, &platformv1.ServiceRestart{
				Policy:            platformv1.RestartPolicy_RESTART_POLICY_ALWAYS,
				MaxRestarts:       4,
				InitialDelayMs:    0,
				MaxDelayMs:        0,
				BackoffMultiplier: 1,
				Jitter:            0,
			}, appliedObs(1), tc.in)
			if decision.Observation.GetLastCause() != tc.cause {
				t.Fatalf("cause = %s, want %s", decision.Observation.GetLastCause(), tc.cause)
			}
		})
	}
}

func TestEvaluateRetryCountBudgetEntersCrashLoop(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	settings := &platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       2,
		WindowSeconds:     0,
		InitialDelayMs:    0,
		MaxDelayMs:        0,
		BackoffMultiplier: 1,
		Jitter:            0,
	}
	obs := appliedObs(1)
	in := Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1}
	var decision Decision
	for i := 0; i < 3; i++ {
		decision = Evaluate(clock.Now(), nil, settings, obs, in)
		obs = decision.Observation
		if decision.Action == ActionStart {
			in = Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1}
			obs.AwaitingRestart = false
		}
	}
	if decision.Action != ActionCrashLoop || !obs.GetCrashLoop() {
		t.Fatalf("expected crash loop, got action=%s obs=%+v", decision.Action, obs)
	}
	decision = Evaluate(clock.Now(), nil, settings, obs, in)
	if decision.Action != ActionCrashLoop {
		t.Fatalf("reconcile after crash loop started again: %s", decision.Action)
	}
}

func TestEvaluateRetryWindowBudgetEntersCrashLoop(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	settings := &platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ALWAYS,
		MaxRestarts:       0,
		WindowSeconds:     30,
		InitialDelayMs:    0,
		MaxDelayMs:        0,
		BackoffMultiplier: 1,
		Jitter:            0,
	}
	obs := appliedObs(1)
	decision := Evaluate(clock.Now(), nil, settings, obs, Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1})
	if decision.Action != ActionStart {
		t.Fatalf("first restart action = %s", decision.Action)
	}
	obs = decision.Observation
	obs.AwaitingRestart = false
	clock.Advance(31 * time.Second)
	decision = Evaluate(clock.Now(), nil, settings, obs, Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1})
	if decision.Action != ActionCrashLoop {
		t.Fatalf("window expiry action = %s, want crash_loop", decision.Action)
	}
}

func TestEvaluateExponentialBackoffWithDeterministicJitter(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	settings := &platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       8,
		InitialDelayMs:    1000,
		MaxDelayMs:        4000,
		BackoffMultiplier: 2,
		Jitter:            0.2,
	}
	obs := appliedObs(1)
	var delays []time.Duration
	for i := 0; i < 4; i++ {
		rng := rand.New(rand.NewSource(7))
		decision := Evaluate(clock.Now(), rng, settings, obs, Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1})
		if decision.Action != ActionWait {
			t.Fatalf("step %d action = %s", i, decision.Action)
		}
		delays = append(delays, decision.Delay)
		obs = decision.Observation
		clock.Set(decision.Observation.GetNextRestartAt().AsTime())
		start := Evaluate(clock.Now(), rng, settings, obs, Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1})
		if start.Action != ActionStart {
			t.Fatalf("step %d after wait action = %s", i, start.Action)
		}
		obs = start.Observation
		obs.AwaitingRestart = false
	}
	if delays[0] < 800*time.Millisecond || delays[0] > 1200*time.Millisecond {
		t.Fatalf("first delay %s outside jittered 1s window", delays[0])
	}
	if delays[3] > 4000*time.Millisecond {
		t.Fatalf("capped delay %s exceeds max", delays[3])
	}
	if delays[2] <= delays[0] {
		t.Fatalf("expected exponential growth, delays=%v", delays)
	}
	rngA := rand.New(rand.NewSource(7))
	rngB := rand.New(rand.NewSource(7))
	first := backoffDelay(CanonicalRestart(settings), 0, rngA)
	second := backoffDelay(CanonicalRestart(settings), 0, rngB)
	if first != second {
		t.Fatalf("jitter is not deterministic: %s vs %s", first, second)
	}
}

func TestEvaluateResetsHistoryAfterStableRun(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	settings := &platformv1.ServiceRestart{
		Policy:             platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:        2,
		StableAfterSeconds: 10,
		InitialDelayMs:     0,
		Jitter:             0,
	}
	obs := appliedObs(1)
	decision := Evaluate(clock.Now(), nil, settings, obs, Input{State: ProcessState{ExitCode: 1}, DesiredRolloutGeneration: 1})
	obs = decision.Observation
	obs.AwaitingRestart = false
	started := Evaluate(clock.Now(), nil, settings, obs, Input{State: ProcessState{Running: true}, DesiredRolloutGeneration: 1})
	if started.Observation.GetRestartCount() != 1 {
		t.Fatalf("count after start = %d", started.Observation.GetRestartCount())
	}
	clock.Advance(10 * time.Second)
	stable := Evaluate(clock.Now(), nil, settings, started.Observation, Input{State: ProcessState{Running: true}, DesiredRolloutGeneration: 1})
	if stable.Observation.GetRestartCount() != 0 || stable.Observation.GetCrashLoop() {
		t.Fatalf("history was not reset: %+v", stable.Observation)
	}
}

func TestEvaluateAuthorizedRestartAndRolloutClearCrashLoop(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	obs := &platformv1.RestartObservation{
		RestartCount:             5,
		CrashLoop:                true,
		AppliedRolloutGeneration: 1,
		LastCause:                platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
		Message:                  "crash loop",
	}
	operator := Evaluate(clock.Now(), nil, nil, obs, Input{
		State:                    ProcessState{ExitCode: 1},
		DesiredRolloutGeneration: 1,
		OperatorRestartNonce:     1,
	})
	if operator.Action != ActionStart || operator.Observation.GetCrashLoop() {
		t.Fatalf("operator restart did not clear crash loop: %+v", operator)
	}
	if operator.Observation.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_OPERATOR {
		t.Fatalf("operator cause = %s", operator.Observation.GetLastCause())
	}
	rollout := Evaluate(clock.Now(), nil, nil, obs, Input{
		State:                    ProcessState{ExitCode: 1},
		DesiredRolloutGeneration: 2,
	})
	if rollout.Action != ActionStart || rollout.Observation.GetCrashLoop() || rollout.Observation.GetRestartCount() != 0 {
		t.Fatalf("rollout did not authorize a fresh start: %+v", rollout)
	}
}

func TestEvaluateDoesNotAdoptCrashLoopAsRunningAfterAgentRestart(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	obs := &platformv1.RestartObservation{
		CrashLoop:                true,
		AppliedRolloutGeneration: 1,
		RestartCount:             5,
	}
	decision := Evaluate(clock.Now(), nil, nil, obs, Input{State: ProcessState{Running: false, ExitCode: 1}, DesiredRolloutGeneration: 1})
	if decision.Action != ActionCrashLoop {
		t.Fatalf("action = %s, want crash_loop", decision.Action)
	}
}

func TestMergeObservationsPrefersNewerAndCrashLoop(t *testing.T) {
	t.Parallel()
	older := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	local := &platformv1.RestartObservation{RestartCount: 1, LastRestartAt: timestamppb.New(newer)}
	remote := &platformv1.RestartObservation{RestartCount: 4, CrashLoop: true, LastRestartAt: timestamppb.New(newer)}
	merged := MergeObservations(local, remote)
	if !merged.GetCrashLoop() || merged.GetRestartCount() != 4 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestClassifyExitPrefersOOMOverSignal(t *testing.T) {
	t.Parallel()
	if got := ClassifyExit(ProcessState{OOMKilled: true, Signal: 9, ExitCode: 137}); got != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyExitPrefersOOMOverDiskExhaustion(t *testing.T) {
	t.Parallel()
	if got := ClassifyExit(ProcessState{OOMKilled: true, DiskExhausted: true, Signal: 9, ExitCode: 137}); got != platformv1.RestartCause_RESTART_CAUSE_OOM_KILL {
		t.Fatalf("got %s", got)
	}
}

func TestEvaluateDiskExhaustionNamesTheCause(t *testing.T) {
	t.Parallel()
	clock := NewManualClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if !Failure(platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED) {
		t.Fatal("disk exhaustion is not a failure")
	}
	if !ShouldRestart(platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE, platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED) {
		t.Fatal("on-failure does not restart disk exhaustion")
	}
	decision := Evaluate(clock.Now(), nil, &platformv1.ServiceRestart{
		Policy:            platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		MaxRestarts:       4,
		InitialDelayMs:    1000,
		MaxDelayMs:        1000,
		BackoffMultiplier: 1,
		Jitter:            0,
	}, appliedObs(1), Input{State: ProcessState{DiskExhausted: true}, DesiredRolloutGeneration: 1})
	if decision.Observation.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED {
		t.Fatalf("cause = %s", decision.Observation.GetLastCause())
	}
	if !strings.Contains(strings.ToLower(decision.Message), "disk exhausted") {
		t.Fatalf("message %q does not name disk exhaustion", decision.Message)
	}
}

func appliedObs(rollout int64) *platformv1.RestartObservation {
	return &platformv1.RestartObservation{AppliedRolloutGeneration: rollout}
}
