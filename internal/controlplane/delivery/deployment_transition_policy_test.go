package delivery

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDeploymentTransitionDecision(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, from, to, actor string
		ignore, changed       bool
		err                   error
	}{
		{"advance", DeploymentStateScheduling, DeploymentStateActive, DeploymentCauseSystem, false, true, nil},
		{"duplicate", DeploymentStateActive, DeploymentStateActive, DeploymentCauseSystem, false, false, nil},
		{"terminal", DeploymentStateFailed, DeploymentStateActive, DeploymentCauseUser, false, false, errDeploymentTerminal},
		{"late agent", DeploymentStateFailed, DeploymentStateActive, DeploymentCauseAgent, false, false, nil},
		{"ignored terminal", DeploymentStateFailed, DeploymentStateActive, DeploymentCauseBuilder, true, false, nil},
		{"backwards", DeploymentStateReadiness, DeploymentStateStarting, DeploymentCauseSystem, false, false, errIllegalDeploymentTransition},
		{"stale agent", DeploymentStateReadiness, DeploymentStateStarting, DeploymentCauseAgent, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := DeploymentRecord{State: tc.from, SpecRevision: 3, ImageDigest: "old"}
			input := deploymentTransitionInput{ToState: tc.to, Actor: deploymentActor{Kind: tc.actor}, IgnoreIfTerminal: tc.ignore, HasSpecRevision: true, SpecRevision: 4, HasImageDigest: true, ImageDigest: "new", Detail: "  ready  "}
			after, changed, err := decideDeploymentTransition(before, input, now)
			if changed != tc.changed || !errors.Is(err, tc.err) {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if !changed && !reflect.DeepEqual(before, after) {
				t.Fatal("ignored transition altered record")
			}
			if changed && (after.SpecRevision != 4 || after.ImageDigest != "new" || after.Detail != "ready" || !after.UpdatedAt.Equal(now) || after.ReasonCode != reasonDeploymentActive) {
				t.Fatalf("transition: %+v", after)
			}
		})
	}
}

func TestAgentDeploymentDecisionPreservesRemoval(t *testing.T) {
	observation := deploymentAgentObservation{Phase: "Healthy", Healthy: true, AgentID: "node", AppliedGeneration: 2, DesiredGeneration: 2}
	for _, phase := range []string{"Healthy", "Failed"} {
		observation.Phase = phase
		observation.Healthy = phase == "Healthy"
		if _, ok := decideAgentDeploymentTransition(DeploymentRecord{State: DeploymentStateDraining, ReasonCode: reasonUserRemove}, observation); ok {
			t.Fatalf("%s overrode removal", phase)
		}
	}
	observation.Phase = "Failed"
	input, ok := decideAgentDeploymentTransition(DeploymentRecord{State: DeploymentStateActive}, observation)
	if !ok || input.ToState != DeploymentStateCrashed || input.Actor.ID != "node" {
		t.Fatalf("crash observation: %+v, %v", input, ok)
	}
}

func TestAgentDeploymentDecisionIgnoresTerminalDeployments(t *testing.T) {
	observation := deploymentAgentObservation{Phase: "Failed", Healthy: false, AgentID: "node", AppliedGeneration: 2, DesiredGeneration: 2}
	for _, state := range []string{
		DeploymentStateCompleted,
		DeploymentStateFailed,
		DeploymentStateCancelled,
		DeploymentStateCrashed,
		DeploymentStateRemoved,
		DeploymentStateSuperseded,
	} {
		if _, ok := decideAgentDeploymentTransition(DeploymentRecord{State: state}, observation); ok {
			t.Fatalf("%s produced a transition", state)
		}
	}
}
