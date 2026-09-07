package delivery

import (
	"testing"
)

func TestDeploymentTransitionAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{name: "idempotent staged", from: DeploymentStateStaged, to: DeploymentStateStaged, want: true},
		{name: "staged to queued build", from: DeploymentStateStaged, to: DeploymentStateQueuedBuild, want: true},
		{name: "staged to scheduling", from: DeploymentStateStaged, to: DeploymentStateScheduling, want: true},
		{name: "staged cannot skip to active", from: DeploymentStateStaged, to: DeploymentStateActive, want: false},
		{name: "queued build to building", from: DeploymentStateQueuedBuild, to: DeploymentStateBuilding, want: true},
		{name: "queued build cannot skip to scheduling", from: DeploymentStateQueuedBuild, to: DeploymentStateScheduling, want: false},
		{name: "building to scheduling", from: DeploymentStateBuilding, to: DeploymentStateScheduling, want: true},
		{name: "building requeue", from: DeploymentStateBuilding, to: DeploymentStateQueuedBuild, want: true},
		{name: "building cannot jump to active", from: DeploymentStateBuilding, to: DeploymentStateActive, want: false},
		{name: "scheduling to image pull", from: DeploymentStateScheduling, to: DeploymentStateImagePull, want: true},
		{name: "scheduling skip to starting", from: DeploymentStateScheduling, to: DeploymentStateStarting, want: true},
		{name: "scheduling skip to readiness", from: DeploymentStateScheduling, to: DeploymentStateReadiness, want: true},
		{name: "scheduling skip to active", from: DeploymentStateScheduling, to: DeploymentStateActive, want: true},
		{name: "image pull to starting", from: DeploymentStateImagePull, to: DeploymentStateStarting, want: true},
		{name: "starting to readiness", from: DeploymentStateStarting, to: DeploymentStateReadiness, want: true},
		{name: "readiness to active", from: DeploymentStateReadiness, to: DeploymentStateActive, want: true},
		{name: "active to draining", from: DeploymentStateActive, to: DeploymentStateDraining, want: true},
		{name: "active failover to scheduling", from: DeploymentStateActive, to: DeploymentStateScheduling, want: true},
		{name: "draining to completed", from: DeploymentStateDraining, to: DeploymentStateCompleted, want: true},
		{name: "active cannot skip to completed", from: DeploymentStateActive, to: DeploymentStateCompleted, want: false},
		{name: "readiness cannot go backward", from: DeploymentStateReadiness, to: DeploymentStateImagePull, want: false},
		{name: "staged to failed", from: DeploymentStateStaged, to: DeploymentStateFailed, want: true},
		{name: "building to cancelled", from: DeploymentStateBuilding, to: DeploymentStateCancelled, want: true},
		{name: "scheduling to superseded", from: DeploymentStateScheduling, to: DeploymentStateSuperseded, want: true},
		{name: "active to removed", from: DeploymentStateActive, to: DeploymentStateRemoved, want: true},
		{name: "starting to crashed", from: DeploymentStateStarting, to: DeploymentStateCrashed, want: true},
		{name: "active to crashed", from: DeploymentStateActive, to: DeploymentStateCrashed, want: true},
		{name: "queued cannot crash", from: DeploymentStateQueuedBuild, to: DeploymentStateCrashed, want: false},
		{name: "failed is terminal", from: DeploymentStateFailed, to: DeploymentStateActive, want: false},
		{name: "cancelled is terminal", from: DeploymentStateCancelled, to: DeploymentStateQueuedBuild, want: false},
		{name: "crashed is terminal", from: DeploymentStateCrashed, to: DeploymentStateActive, want: false},
		{name: "completed is terminal", from: DeploymentStateCompleted, to: DeploymentStateActive, want: false},
		{name: "removed is terminal", from: DeploymentStateRemoved, to: DeploymentStateStaged, want: false},
		{name: "superseded is terminal", from: DeploymentStateSuperseded, to: DeploymentStateBuilding, want: false},
		{name: "failed stays failed", from: DeploymentStateFailed, to: DeploymentStateFailed, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deploymentTransitionAllowed(tt.from, tt.to); got != tt.want {
				t.Fatalf("deploymentTransitionAllowed(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestAgentObservedDeploymentState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		phase    string
		healthy  bool
		applied  int64
		desired  int64
		want     string
		wantOK   bool
		current  string
		resolved string
	}{
		{name: "healthy phase", phase: "Healthy", healthy: true, applied: 2, desired: 2, want: DeploymentStateActive, wantOK: true, current: DeploymentStateReadiness, resolved: DeploymentStateActive},
		{name: "pending while applying", phase: "Pending", applied: 1, desired: 2, want: DeploymentStateImagePull, wantOK: true, current: DeploymentStateScheduling, resolved: DeploymentStateImagePull},
		{name: "starting before applied", phase: "Starting", applied: 1, desired: 2, want: DeploymentStateStarting, wantOK: true, current: DeploymentStateImagePull, resolved: DeploymentStateStarting},
		{name: "starting after applied", phase: "Starting", applied: 2, desired: 2, want: DeploymentStateReadiness, wantOK: true, current: DeploymentStateStarting, resolved: DeploymentStateReadiness},
		{name: "error before active is failed", phase: "Error", applied: 2, desired: 2, want: DeploymentStateFailed, wantOK: true, current: DeploymentStateScheduling, resolved: DeploymentStateFailed},
		{name: "error after active is crashed", phase: "Error", applied: 2, desired: 2, want: DeploymentStateFailed, wantOK: true, current: DeploymentStateActive, resolved: DeploymentStateCrashed},
		{name: "crash loop is crashed", phase: "CrashLoop", applied: 2, desired: 2, want: DeploymentStateCrashed, wantOK: true, current: DeploymentStateActive, resolved: DeploymentStateCrashed},
		{name: "backoff stays starting", phase: "Backoff", applied: 2, desired: 2, want: DeploymentStateStarting, wantOK: true, current: DeploymentStateStarting, resolved: DeploymentStateStarting},
		{name: "stopped after start is crashed", phase: "Stopped", applied: 2, desired: 2, want: DeploymentStateFailed, wantOK: true, current: DeploymentStateActive, resolved: DeploymentStateCrashed},
		{name: "unknown phase ignored", phase: "Mystery", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := agentObservedDeploymentState(tt.phase, tt.healthy, tt.applied, tt.desired)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("agentObservedDeploymentState = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if resolved := resolveAgentTargetState(tt.current, got); resolved != tt.resolved {
				t.Fatalf("resolveAgentTargetState = %q, want %q", resolved, tt.resolved)
			}
		})
	}
}
