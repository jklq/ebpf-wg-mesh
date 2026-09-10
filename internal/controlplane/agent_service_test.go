package controlplane

import (
	"context"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubLiveOwner struct {
	held bool
	addr string
	err  error
}

func (s stubLiveOwner) Lookup(context.Context) (bool, string, error) {
	return s.held, s.addr, s.err
}

func TestAgentServiceRedirectsNonOwnerRPCs(t *testing.T) {
	t.Parallel()

	service := NewAgentService(nil, nil, nil, nil, nil, nil, true, "agent-trusted", "dashboard-1",
		WithLiveOwner(stubLiveOwner{held: false, addr: "owner:9443"}))
	_, err := service.Enroll(context.Background(), &agentv1.EnrollRequest{AgentId: "agent-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Enroll status = %v", err)
	}
	if got, ok := parseLiveOwner(err); !ok || got != "owner:9443" {
		t.Fatalf("Enroll redirect = %q, %v", got, err)
	}

	_, err = service.IssueManagedDashboardCertificate(context.Background(), &agentv1.ManagedDashboardCertificateRequest{AgentId: "agent-trusted"})
	if got, ok := parseLiveOwner(err); !ok || got != "owner:9443" {
		t.Fatalf("dashboard redirect = %q, %v", got, err)
	}

	unavailable := NewAgentService(nil, nil, nil, nil, nil, nil, false, "", "",
		WithLiveOwner(stubLiveOwner{held: false, addr: ""}))
	_, err = unavailable.Enroll(context.Background(), &agentv1.EnrollRequest{AgentId: "agent-1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("missing owner status = %v", err)
	}
}

func parseLiveOwner(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || !strings.HasPrefix(st.Message(), agentv1.LiveOwnerRedirectPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(st.Message(), agentv1.LiveOwnerRedirectPrefix)), true
}

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

	lastRevision, lastReplicas, err := sendLatestDesiredState(
		context.Background(),
		"agent-1",
		1,
		nil,
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
	if lastReplicas != nil {
		t.Fatalf("expected no replica metadata, got %v", lastReplicas)
	}
	if len(sent) != 2 || sent[0] != 2 || sent[1] != 3 {
		t.Fatalf("expected revisions [2 3], got %v", sent)
	}
}

func TestSendLatestDesiredStateResendsWhenReplicaAddressesChange(t *testing.T) {
	t.Parallel()

	states := []*agentv1.DesiredNodeState{
		{ReconciliationCursor: 3, ReplicaAddresses: []string{"replica-a:9443", "replica-b:9443"}},
		{ReconciliationCursor: 3, ReplicaAddresses: []string{"replica-a:9443", "replica-b:9443"}},
	}
	var loads int
	var sent [][]string

	lastRevision, lastReplicas, err := sendLatestDesiredState(
		context.Background(),
		"agent-1",
		3,
		[]string{"replica-a:9443"},
		func() (*agentv1.DesiredNodeState, error) {
			if loads >= len(states) {
				return states[len(states)-1], nil
			}
			state := states[loads]
			loads++
			return state, nil
		},
		func(state *agentv1.DesiredNodeState) error {
			sent = append(sent, append([]string(nil), state.GetReplicaAddresses()...))
			return nil
		},
	)
	if err != nil {
		t.Fatalf("sendLatestDesiredState: %v", err)
	}
	if lastRevision != 3 {
		t.Fatalf("expected last revision 3, got %d", lastRevision)
	}
	if got, want := strings.Join(lastReplicas, ","), "replica-a:9443,replica-b:9443"; got != want {
		t.Fatalf("last replicas = %q, want %q", got, want)
	}
	if len(sent) != 1 || strings.Join(sent[0], ",") != "replica-a:9443,replica-b:9443" {
		t.Fatalf("expected one replica-metadata resend, got %v", sent)
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
