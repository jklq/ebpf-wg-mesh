package controlplane

import (
	"context"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestSendLatestDesiredStateDrainsNewerRevisions(t *testing.T) {
	t.Parallel()

	states := []*agentv1.DesiredNodeState{
		{ReconciliationCursor: 2},
		{
			ReconciliationCursor: 3,
			Services: []*agentv1.DesiredService{{
				AllocationId:             "alloc-1",
				ServiceId:                "svc-1",
				DesiredSpecRevision:      1,
				DesiredRolloutGeneration: 2,
			}},
		},
		{ReconciliationCursor: 3},
	}
	var loads int
	var sent []int64

	lastRevision, err := sendLatestDesiredState(
		context.Background(),
		"agent-1",
		1,
		func() (*agentv1.DesiredNodeState, error) {
			if loads >= len(states) {
				return states[len(states)-1], nil
			}
			state := states[loads]
			loads++
			return state, nil
		},
		func(state *agentv1.DesiredNodeState) error {
			sent = append(sent, state.GetReconciliationCursor())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("sendLatestDesiredState: %v", err)
	}
	if lastRevision != 3 {
		t.Fatalf("expected last revision 3, got %d", lastRevision)
	}
	if len(sent) != 2 || sent[0] != 2 || sent[1] != 3 {
		t.Fatalf("expected revisions [2 3], got %v", sent)
	}
}

func TestAgentServiceAttachesExactPullCredentialOnlyToPlatformImages(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	auth, err := NewRegistryAuth(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := &AgentService{registry: NewRegistryPolicy(cfg, auth)}
	state := &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{
		{
			AllocationId:  "allocation-1",
			ServiceId:     "service-1",
			EnvironmentId: "environment-1",
			Spec: &platformv1.ResolvedServiceSpec{
				Image: "registry.example.test:5000/mesh/project-1/environment-1/build-1/service-1@sha256:" + strings.Repeat("a", 64),
			},
		},
		{
			AllocationId:  "allocation-2",
			ServiceId:     "service-2",
			EnvironmentId: "environment-2",
			Spec:          &platformv1.ResolvedServiceSpec{Image: "docker.io/library/nginx:latest"},
		},
	}}
	if err := service.attachRegistryPullCredentials("agent-1", state); err != nil {
		t.Fatal(err)
	}
	managed := state.Services[0]
	if managed.GetRegistryUsername() == "" || managed.GetRegistryPassword() == "" {
		t.Fatal("platform image did not receive pull credentials")
	}
	claims, err := auth.parseCapability(managed.GetRegistryUsername(), managed.GetRegistryPassword())
	if err != nil {
		t.Fatal(err)
	}
	if got := claims.Access[0]; got.Name != "mesh/project-1/environment-1/build-1/service-1" || !sameStrings(got.Actions, []string{"pull"}) {
		t.Fatalf("unexpected pull scope %+v", got)
	}
	if external := state.Services[1]; external.GetRegistryUsername() != "" || external.GetRegistryPassword() != "" {
		t.Fatal("external direct image received platform registry credentials")
	}
	foreign := &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{{
		AllocationId:  "allocation-3",
		EnvironmentId: "environment-2",
		ServiceId:     "service-2",
		Spec: &platformv1.ResolvedServiceSpec{
			Image: "registry.example.test:5000/mesh/project-1/environment-1/build-1/service-1@sha256:" + strings.Repeat("b", 64),
		},
	}}}
	if err := service.attachRegistryPullCredentials("agent-1", foreign); err == nil {
		t.Fatal("expected a sibling platform repository to be rejected")
	}
}
