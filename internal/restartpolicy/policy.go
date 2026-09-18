package restartpolicy

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	PhaseCrashLoop = "CrashLoop"
	PhaseStopped   = "Stopped"
	PhaseBackoff   = "Backoff"

	DefaultPolicy            = platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	DefaultMaxRestarts       = int32(5)
	DefaultWindowSeconds     = int32(300)
	DefaultInitialDelayMS    = int32(1000)
	DefaultMaxDelayMS        = int32(60000)
	DefaultBackoffMultiplier = 2.0
	DefaultJitter            = 0.1
	DefaultStableAfterSecs   = int32(60)
)

type Action int

const (
	ActionKeep Action = iota
	ActionStart
	ActionWait
	ActionStop
	ActionCrashLoop
)

func (a Action) String() string {
	switch a {
	case ActionKeep:
		return "keep"
	case ActionStart:
		return "start"
	case ActionWait:
		return "wait"
	case ActionStop:
		return "stop"
	case ActionCrashLoop:
		return "crash_loop"
	default:
		return "unknown"
	}
}

type ProcessState struct {
	Running       bool
	ExitCode      int32
	Signal        int32
	OOMKilled     bool
	DiskExhausted bool
}

type Input struct {
	State                    ProcessState
	LivenessFailed           bool
	DesiredRolloutGeneration int64
	OperatorRestartNonce     int64
}

type Decision struct {
	Action      Action
	Observation *platformv1.RestartObservation
	Phase       string
	Message     string
	Delay       time.Duration
}

func CanonicalRestart(restart *platformv1.ServiceRestart) *platformv1.ServiceRestart {
	if restart == nil {
		restart = &platformv1.ServiceRestart{}
	} else {
		restart = proto.Clone(restart).(*platformv1.ServiceRestart)
	}
	if restart.Policy == platformv1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		restart.Policy = DefaultPolicy
	}
	if restart.Policy != platformv1.RestartPolicy_RESTART_POLICY_NEVER &&
		restart.MaxRestarts == 0 && restart.WindowSeconds == 0 {
		restart.MaxRestarts = DefaultMaxRestarts
		restart.WindowSeconds = DefaultWindowSeconds
	}
	if restart.InitialDelayMs == 0 && restart.MaxDelayMs == 0 && restart.BackoffMultiplier == 0 && restart.Jitter == 0 {
		restart.InitialDelayMs = DefaultInitialDelayMS
		restart.MaxDelayMs = DefaultMaxDelayMS
		restart.BackoffMultiplier = DefaultBackoffMultiplier
		restart.Jitter = DefaultJitter
	}
	if restart.BackoffMultiplier == 0 {
		restart.BackoffMultiplier = DefaultBackoffMultiplier
	}
	if restart.StableAfterSeconds == 0 {
		restart.StableAfterSeconds = DefaultStableAfterSecs
	}
	return restart
}

func ValidateRestart(restart *platformv1.ServiceRestart) error {
	if restart == nil {
		return nil
	}
	switch restart.GetPolicy() {
	case platformv1.RestartPolicy_RESTART_POLICY_UNSPECIFIED,
		platformv1.RestartPolicy_RESTART_POLICY_ALWAYS,
		platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		platformv1.RestartPolicy_RESTART_POLICY_NEVER:
	default:
		return fmt.Errorf("unsupported restart policy %s", restart.GetPolicy())
	}
	if restart.GetMaxRestarts() < 0 {
		return fmt.Errorf("max_restarts must be non-negative")
	}
	if restart.GetWindowSeconds() < 0 {
		return fmt.Errorf("window_seconds must be non-negative")
	}
	if restart.GetInitialDelayMs() < 0 {
		return fmt.Errorf("initial_delay_ms must be non-negative")
	}
	if restart.GetMaxDelayMs() < 0 {
		return fmt.Errorf("max_delay_ms must be non-negative")
	}
	if restart.GetMaxDelayMs() > 0 && restart.GetInitialDelayMs() > restart.GetMaxDelayMs() {
		return fmt.Errorf("max_delay_ms must be greater than or equal to initial_delay_ms")
	}
	if restart.GetBackoffMultiplier() < 0 {
		return fmt.Errorf("backoff_multiplier must be non-negative")
	}
	if restart.GetJitter() < 0 || restart.GetJitter() > 1 {
		return fmt.Errorf("jitter must be between 0 and 1")
	}
	if restart.GetStableAfterSeconds() < 0 {
		return fmt.Errorf("stable_after_seconds must be non-negative")
	}
	return nil
}

func ClassifyExit(state ProcessState) platformv1.RestartCause {
	if state.OOMKilled {
		return platformv1.RestartCause_RESTART_CAUSE_OOM_KILL
	}
	if state.DiskExhausted {
		return platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED
	}
	if state.Signal != 0 {
		return platformv1.RestartCause_RESTART_CAUSE_SIGNAL
	}
	if state.ExitCode == 0 {
		return platformv1.RestartCause_RESTART_CAUSE_EXIT_ZERO
	}
	return platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO
}

func Failure(cause platformv1.RestartCause) bool {
	switch cause {
	case platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO,
		platformv1.RestartCause_RESTART_CAUSE_SIGNAL,
		platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
		platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED,
		platformv1.RestartCause_RESTART_CAUSE_LIVENESS:
		return true
	default:
		return false
	}
}

func ShouldRestart(policy platformv1.RestartPolicy, cause platformv1.RestartCause) bool {
	switch policy {
	case platformv1.RestartPolicy_RESTART_POLICY_NEVER:
		return false
	case platformv1.RestartPolicy_RESTART_POLICY_ALWAYS:
		return true
	default:
		return Failure(cause)
	}
}

func MergeObservations(local, remote *platformv1.RestartObservation) *platformv1.RestartObservation {
	if local == nil || isEmptyObservation(local) {
		return cloneObservation(remote)
	}
	if remote == nil || isEmptyObservation(remote) {
		return cloneObservation(local)
	}
	localTime := timeFromProto(local.GetLastRestartAt())
	remoteTime := timeFromProto(remote.GetLastRestartAt())
	if remote.GetCrashLoop() && !local.GetCrashLoop() && !remoteTime.Before(localTime) {
		return cloneObservation(remote)
	}
	if local.GetCrashLoop() && !remote.GetCrashLoop() && !localTime.Before(remoteTime) {
		return cloneObservation(local)
	}
	if remoteTime.After(localTime) {
		return cloneObservation(remote)
	}
	if localTime.After(remoteTime) {
		return cloneObservation(local)
	}
	if remote.GetRestartCount() > local.GetRestartCount() {
		return cloneObservation(remote)
	}
	return cloneObservation(local)
}

func Evaluate(now time.Time, rng *rand.Rand, restart *platformv1.ServiceRestart, obs *platformv1.RestartObservation, in Input) Decision {
	now = now.UTC()
	settings := CanonicalRestart(restart)
	current := cloneObservation(obs)
	if current == nil {
		current = &platformv1.RestartObservation{}
	}

	if in.DesiredRolloutGeneration > current.GetAppliedRolloutGeneration() ||
		in.OperatorRestartNonce > current.GetAppliedOperatorRestartNonce() {
		if in.State.Running && !in.LivenessFailed && !current.GetCrashLoop() &&
			current.GetAppliedRolloutGeneration() == 0 && current.GetAppliedOperatorRestartNonce() == 0 &&
			current.GetRestartCount() == 0 {
			current.AppliedRolloutGeneration = in.DesiredRolloutGeneration
			current.AppliedOperatorRestartNonce = in.OperatorRestartNonce
			if current.GetStartedAt() == nil || current.GetStartedAt().AsTime().IsZero() {
				current.StartedAt = timestamppb.New(now)
			}
			return Decision{Action: ActionKeep, Observation: current, Message: current.GetMessage()}
		}
		current = authorizedReset(current, in, now)
		if in.DesiredRolloutGeneration > 0 {
			current.AppliedRolloutGeneration = in.DesiredRolloutGeneration
		}
		current.AppliedOperatorRestartNonce = in.OperatorRestartNonce
		if in.OperatorRestartNonce > obsNonce(obs) {
			current.LastCause = platformv1.RestartCause_RESTART_CAUSE_OPERATOR
			current.Message = "operator restart requested"
		} else {
			current.Message = "new rollout authorized a fresh start"
		}
		return Decision{Action: ActionStart, Observation: current, Phase: "Pending", Message: current.Message}
	}

	if in.State.Running && !in.LivenessFailed {
		if current.GetStartedAt() == nil || current.GetStartedAt().AsTime().IsZero() {
			current.StartedAt = timestamppb.New(now)
		}
		current.AwaitingRestart = false
		current.CrashLoop = false
		if settings.GetStableAfterSeconds() > 0 {
			started := current.GetStartedAt().AsTime().UTC()
			if !started.IsZero() && !now.Before(started.Add(time.Duration(settings.GetStableAfterSeconds())*time.Second)) {
				current = resetCrashHistory(current)
				current.StartedAt = timestamppb.New(started)
				current.Message = "crash history reset after a stable run"
			}
		}
		current.AppliedRolloutGeneration = in.DesiredRolloutGeneration
		current.AppliedOperatorRestartNonce = in.OperatorRestartNonce
		return Decision{Action: ActionKeep, Observation: current, Message: current.GetMessage()}
	}

	if current.GetCrashLoop() {
		return crashLoopDecision(current, "retry budget exhausted; authorized restart or new rollout required")
	}

	cause := current.GetLastCause()
	if in.LivenessFailed {
		cause = platformv1.RestartCause_RESTART_CAUSE_LIVENESS
	} else if !in.State.Running {
		cause = ClassifyExit(in.State)
	}

	if !current.GetAwaitingRestart() {
		current.LastCause = cause
		if !in.LivenessFailed {
			current.LastExitCode = in.State.ExitCode
			current.LastSignal = in.State.Signal
		}
		if !ShouldRestart(settings.GetPolicy(), cause) {
			current.AwaitingRestart = false
			current.CrashLoop = false
			current.Message = stopMessage(settings.GetPolicy(), cause)
			return Decision{Action: ActionStop, Observation: current, Phase: PhaseStopped, Message: current.Message}
		}
		if budgetExhausted(settings, current, now) {
			current.CrashLoop = true
			current.AwaitingRestart = false
			current.Message = crashLoopMessage(settings, current, cause)
			return crashLoopDecision(current, current.Message)
		}
		delay := backoffDelay(settings, current.GetRestartCount(), rng)
		current.RestartCount++
		if current.GetWindowStartedAt() == nil || current.GetWindowStartedAt().AsTime().IsZero() {
			current.WindowStartedAt = timestamppb.New(now)
		}
		current.LastRestartAt = timestamppb.New(now)
		current.NextRestartAt = timestamppb.New(now.Add(delay))
		current.AwaitingRestart = true
		current.StartedAt = nil
		current.Message = fmt.Sprintf("restarting after %s (attempt %d)", causeLabel(cause), current.GetRestartCount())
		if delay > 0 {
			return Decision{
				Action:      ActionWait,
				Observation: current,
				Phase:       PhaseBackoff,
				Message:     current.Message,
				Delay:       delay,
			}
		}
		return Decision{Action: ActionStart, Observation: current, Phase: "Pending", Message: current.Message, Delay: delay}
	}

	next := timeFromProto(current.GetNextRestartAt())
	if !next.IsZero() && now.Before(next) {
		return Decision{
			Action:      ActionWait,
			Observation: current,
			Phase:       PhaseBackoff,
			Message:     current.GetMessage(),
			Delay:       next.Sub(now),
		}
	}
	current.Message = fmt.Sprintf("restarting after %s (attempt %d)", causeLabel(current.GetLastCause()), current.GetRestartCount())
	return Decision{Action: ActionStart, Observation: current, Phase: "Pending", Message: current.Message}
}

func budgetExhausted(settings *platformv1.ServiceRestart, obs *platformv1.RestartObservation, now time.Time) bool {
	window := time.Duration(settings.GetWindowSeconds()) * time.Second
	started := timeFromProto(obs.GetWindowStartedAt())
	if window > 0 && !started.IsZero() && !now.Before(started.Add(window)) && obs.GetRestartCount() > 0 {
		return true
	}
	if settings.GetMaxRestarts() > 0 && obs.GetRestartCount() >= settings.GetMaxRestarts() {
		return true
	}
	return false
}

func backoffDelay(settings *platformv1.ServiceRestart, completedRestarts int32, rng *rand.Rand) time.Duration {
	initial := time.Duration(settings.GetInitialDelayMs()) * time.Millisecond
	maxDelay := time.Duration(settings.GetMaxDelayMs()) * time.Millisecond
	if initial < 0 {
		initial = 0
	}
	if maxDelay > 0 && initial > maxDelay {
		initial = maxDelay
	}
	multiplier := settings.GetBackoffMultiplier()
	if multiplier <= 0 {
		multiplier = 1
	}
	base := float64(initial)
	if completedRestarts > 0 {
		base *= math.Pow(multiplier, float64(completedRestarts))
	}
	if maxDelay > 0 && base > float64(maxDelay) {
		base = float64(maxDelay)
	}
	jitter := settings.GetJitter()
	if jitter > 0 && rng != nil && base > 0 {
		factor := 1 + (2*rng.Float64()-1)*jitter
		if factor < 0 {
			factor = 0
		}
		base *= factor
	}
	if maxDelay > 0 && base > float64(maxDelay) {
		base = float64(maxDelay)
	}
	return time.Duration(base)
}

func FormatRestart(restart *platformv1.ServiceRestart) string {
	restart = CanonicalRestart(restart)
	var b strings.Builder
	b.WriteString(policyLabel(restart.GetPolicy()))
	if restart.GetMaxRestarts() > 0 {
		fmt.Fprintf(&b, ", max %d", restart.GetMaxRestarts())
	}
	if restart.GetWindowSeconds() > 0 {
		fmt.Fprintf(&b, ", window %ds", restart.GetWindowSeconds())
	}
	fmt.Fprintf(&b, ", backoff %dms-%dms x%g ±%.0f%%",
		restart.GetInitialDelayMs(), restart.GetMaxDelayMs(), restart.GetBackoffMultiplier(), restart.GetJitter()*100)
	if restart.GetStableAfterSeconds() > 0 {
		fmt.Fprintf(&b, ", stable after %ds", restart.GetStableAfterSeconds())
	}
	return b.String()
}

func cloneObservation(obs *platformv1.RestartObservation) *platformv1.RestartObservation {
	if obs == nil {
		return nil
	}
	return proto.Clone(obs).(*platformv1.RestartObservation)
}

func authorizedReset(prev *platformv1.RestartObservation, in Input, now time.Time) *platformv1.RestartObservation {
	next := &platformv1.RestartObservation{
		AppliedRolloutGeneration:    in.DesiredRolloutGeneration,
		AppliedOperatorRestartNonce: in.OperatorRestartNonce,
		LastRestartAt:               timestamppb.New(now),
	}
	if prev != nil && in.OperatorRestartNonce > prev.GetAppliedOperatorRestartNonce() {
		next.LastCause = platformv1.RestartCause_RESTART_CAUSE_OPERATOR
	}
	return next
}

func resetCrashHistory(obs *platformv1.RestartObservation) *platformv1.RestartObservation {
	next := cloneObservation(obs)
	if next == nil {
		next = &platformv1.RestartObservation{}
	}
	next.RestartCount = 0
	next.WindowStartedAt = nil
	next.NextRestartAt = nil
	next.CrashLoop = false
	next.AwaitingRestart = false
	return next
}

func crashLoopDecision(obs *platformv1.RestartObservation, message string) Decision {
	obs.CrashLoop = true
	obs.AwaitingRestart = false
	if message != "" {
		obs.Message = message
	}
	return Decision{Action: ActionCrashLoop, Observation: obs, Phase: PhaseCrashLoop, Message: obs.Message}
}

func crashLoopMessage(settings *platformv1.ServiceRestart, obs *platformv1.RestartObservation, cause platformv1.RestartCause) string {
	return fmt.Sprintf("crash loop after %s (%d restarts, policy %s)", causeLabel(cause), obs.GetRestartCount(), policyLabel(settings.GetPolicy()))
}

func stopMessage(policy platformv1.RestartPolicy, cause platformv1.RestartCause) string {
	if policy == platformv1.RestartPolicy_RESTART_POLICY_NEVER {
		return "process exited; restart policy is never"
	}
	return fmt.Sprintf("process exited (%s); on-failure policy does not restart", causeLabel(cause))
}

func causeLabel(cause platformv1.RestartCause) string {
	switch cause {
	case platformv1.RestartCause_RESTART_CAUSE_EXIT_ZERO:
		return "exit code 0"
	case platformv1.RestartCause_RESTART_CAUSE_EXIT_NONZERO:
		return "non-zero exit"
	case platformv1.RestartCause_RESTART_CAUSE_SIGNAL:
		return "signal"
	case platformv1.RestartCause_RESTART_CAUSE_OOM_KILL:
		return "OOM kill"
	case platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED:
		return "disk exhausted"
	case platformv1.RestartCause_RESTART_CAUSE_LIVENESS:
		return "liveness restart"
	case platformv1.RestartCause_RESTART_CAUSE_OPERATOR:
		return "operator restart"
	case platformv1.RestartCause_RESTART_CAUSE_NODE_LOSS:
		return "node loss"
	default:
		return "unspecified"
	}
}

func policyLabel(policy platformv1.RestartPolicy) string {
	switch policy {
	case platformv1.RestartPolicy_RESTART_POLICY_ALWAYS:
		return "always"
	case platformv1.RestartPolicy_RESTART_POLICY_NEVER:
		return "never"
	default:
		return "on-failure"
	}
}

func timeFromProto(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

func obsNonce(obs *platformv1.RestartObservation) int64 {
	if obs == nil {
		return 0
	}
	return obs.GetAppliedOperatorRestartNonce()
}

func isEmptyObservation(obs *platformv1.RestartObservation) bool {
	if obs == nil {
		return true
	}
	return obs.GetRestartCount() == 0 &&
		!obs.GetCrashLoop() &&
		!obs.GetAwaitingRestart() &&
		obs.GetLastCause() == platformv1.RestartCause_RESTART_CAUSE_UNSPECIFIED &&
		obs.GetLastRestartAt() == nil &&
		obs.GetStartedAt() == nil &&
		obs.GetAppliedRolloutGeneration() == 0 &&
		obs.GetAppliedOperatorRestartNonce() == 0
}
