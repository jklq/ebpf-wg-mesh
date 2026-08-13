package controlplane

import "testing"

func TestDeploymentTransitionAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{name: "idempotent staged", from: deploymentStateStaged, to: deploymentStateStaged, want: true},
		{name: "staged to queued build", from: deploymentStateStaged, to: deploymentStateQueuedBuild, want: true},
		{name: "staged to scheduling", from: deploymentStateStaged, to: deploymentStateScheduling, want: true},
		{name: "staged cannot skip to active", from: deploymentStateStaged, to: deploymentStateActive, want: false},
		{name: "queued build to building", from: deploymentStateQueuedBuild, to: deploymentStateBuilding, want: true},
		{name: "queued build cannot skip to scheduling", from: deploymentStateQueuedBuild, to: deploymentStateScheduling, want: false},
		{name: "building to scheduling", from: deploymentStateBuilding, to: deploymentStateScheduling, want: true},
		{name: "building requeue", from: deploymentStateBuilding, to: deploymentStateQueuedBuild, want: true},
		{name: "building cannot jump to active", from: deploymentStateBuilding, to: deploymentStateActive, want: false},
		{name: "scheduling to image pull", from: deploymentStateScheduling, to: deploymentStateImagePull, want: true},
		{name: "scheduling skip to starting", from: deploymentStateScheduling, to: deploymentStateStarting, want: true},
		{name: "scheduling skip to readiness", from: deploymentStateScheduling, to: deploymentStateReadiness, want: true},
		{name: "scheduling skip to active", from: deploymentStateScheduling, to: deploymentStateActive, want: true},
		{name: "image pull to starting", from: deploymentStateImagePull, to: deploymentStateStarting, want: true},
		{name: "starting to readiness", from: deploymentStateStarting, to: deploymentStateReadiness, want: true},
		{name: "readiness to active", from: deploymentStateReadiness, to: deploymentStateActive, want: true},
		{name: "active to draining", from: deploymentStateActive, to: deploymentStateDraining, want: true},
		{name: "active failover to scheduling", from: deploymentStateActive, to: deploymentStateScheduling, want: true},
		{name: "draining to completed", from: deploymentStateDraining, to: deploymentStateCompleted, want: true},
		{name: "active cannot skip to completed", from: deploymentStateActive, to: deploymentStateCompleted, want: false},
		{name: "readiness cannot go backward", from: deploymentStateReadiness, to: deploymentStateImagePull, want: false},
		{name: "staged to failed", from: deploymentStateStaged, to: deploymentStateFailed, want: true},
		{name: "building to cancelled", from: deploymentStateBuilding, to: deploymentStateCancelled, want: true},
		{name: "scheduling to superseded", from: deploymentStateScheduling, to: deploymentStateSuperseded, want: true},
		{name: "active to removed", from: deploymentStateActive, to: deploymentStateRemoved, want: true},
		{name: "starting to crashed", from: deploymentStateStarting, to: deploymentStateCrashed, want: true},
		{name: "active to crashed", from: deploymentStateActive, to: deploymentStateCrashed, want: true},
		{name: "queued cannot crash", from: deploymentStateQueuedBuild, to: deploymentStateCrashed, want: false},
		{name: "failed is terminal", from: deploymentStateFailed, to: deploymentStateActive, want: false},
		{name: "cancelled is terminal", from: deploymentStateCancelled, to: deploymentStateQueuedBuild, want: false},
		{name: "crashed is terminal", from: deploymentStateCrashed, to: deploymentStateActive, want: false},
		{name: "completed is terminal", from: deploymentStateCompleted, to: deploymentStateActive, want: false},
		{name: "removed is terminal", from: deploymentStateRemoved, to: deploymentStateStaged, want: false},
		{name: "superseded is terminal", from: deploymentStateSuperseded, to: deploymentStateBuilding, want: false},
		{name: "failed stays failed", from: deploymentStateFailed, to: deploymentStateFailed, want: true},
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
		{name: "healthy phase", phase: "Healthy", healthy: true, applied: 2, desired: 2, want: deploymentStateActive, wantOK: true, current: deploymentStateReadiness, resolved: deploymentStateActive},
		{name: "pending while applying", phase: "Pending", applied: 1, desired: 2, want: deploymentStateImagePull, wantOK: true, current: deploymentStateScheduling, resolved: deploymentStateImagePull},
		{name: "starting before applied", phase: "Starting", applied: 1, desired: 2, want: deploymentStateStarting, wantOK: true, current: deploymentStateImagePull, resolved: deploymentStateStarting},
		{name: "starting after applied", phase: "Starting", applied: 2, desired: 2, want: deploymentStateReadiness, wantOK: true, current: deploymentStateStarting, resolved: deploymentStateReadiness},
		{name: "error before active is failed", phase: "Error", applied: 2, desired: 2, want: deploymentStateFailed, wantOK: true, current: deploymentStateScheduling, resolved: deploymentStateFailed},
		{name: "error after active is crashed", phase: "Error", applied: 2, desired: 2, want: deploymentStateFailed, wantOK: true, current: deploymentStateActive, resolved: deploymentStateCrashed},
		{name: "crash loop is crashed", phase: "CrashLoop", applied: 2, desired: 2, want: deploymentStateCrashed, wantOK: true, current: deploymentStateActive, resolved: deploymentStateCrashed},
		{name: "backoff stays starting", phase: "Backoff", applied: 2, desired: 2, want: deploymentStateStarting, wantOK: true, current: deploymentStateStarting, resolved: deploymentStateStarting},
		{name: "stopped after start is crashed", phase: "Stopped", applied: 2, desired: 2, want: deploymentStateFailed, wantOK: true, current: deploymentStateActive, resolved: deploymentStateCrashed},
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
