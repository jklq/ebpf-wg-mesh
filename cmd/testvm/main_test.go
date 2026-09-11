package main

import (
	"reflect"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type stubPlatformClient struct {
	platformv1.PlatformServiceClient
	id string
}

func TestValidateAgentBindings(t *testing.T) {
	hosts := map[string]hostInfo{
		"controlplane": {Name: "controlplane"},
		"agent-a":      {Name: "agent-a"},
		"agent-b":      {Name: "agent-b"},
	}
	cases := []struct {
		name    string
		tokens  map[string]string
		wantErr bool
	}{
		{"matching hosts and tokens", map[string]string{"agent-a": "token-a", "agent-b": "token-b"}, false},
		{"host without token", map[string]string{"agent-a": "token-a"}, true},
		{"token without host", map[string]string{"agent-a": "token-a", "agent-b": "token-b", "agent-c": "token-c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentBindings(hosts, tc.tokens)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAgentBindings() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestStressPlanAgentNamesMatchProviderTopology(t *testing.T) {
	tests := []struct {
		provider string
		agents   int
		want     []string
	}{
		{provider: "hetzner", agents: 2, want: []string{"agent-a", "agent-b"}},
		{provider: "ovh", agents: 3, want: []string{"agent-01", "agent-02", "agent-03"}},
		{provider: "local", agents: 2, want: []string{"agent-01", "agent-02"}},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			got := stressPlanAgentNames(tc.provider, tc.agents)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("names = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCrossReplicaLiveReaderUsesOwner(t *testing.T) {
	owner := &stubPlatformClient{id: "owner"}
	replica := &stubPlatformClient{id: "replica"}
	clients := crossReplicaClients{owner: owner, replica: replica}
	if got := clients.liveReader(); got != platformv1.PlatformServiceClient(owner) {
		t.Fatal("liveReader() must read delivery live state through the owner, not the replica")
	}
}

func TestMatchingHealthyAllocation(t *testing.T) {
	starting := &platformv1.AllocationStatus{
		AllocationId: "starting", AgentId: "agent-a",
		Healthy: true, RolloutState: "starting",
		AppliedSpecRevision: 2, AppliedRolloutGeneration: 2,
		AllocationIpv4: "10.0.0.6", HealthyIpv4Ports: []int32{8080},
	}
	noEndpoints := &platformv1.AllocationStatus{
		AllocationId: "no-endpoints", AgentId: "agent-a",
		Healthy: true, RolloutState: "serving",
		AppliedSpecRevision: 2, AppliedRolloutGeneration: 2,
		AllocationIpv4: "10.0.0.7",
	}
	healthy := &platformv1.AllocationStatus{
		AllocationId: "healthy", AgentId: "agent-a",
		Healthy: true, RolloutState: "serving",
		AppliedSpecRevision: 2, AppliedRolloutGeneration: 2,
		AllocationIpv4: "10.0.0.5", HealthyIpv4Ports: []int32{8080},
	}
	otherAgent := &platformv1.AllocationStatus{
		AllocationId: "other", AgentId: "agent-b",
		Healthy: true, RolloutState: "serving",
		AppliedSpecRevision: 2, AppliedRolloutGeneration: 2,
		AllocationIpv4: "10.0.0.8", HealthyIpv4Ports: []int32{8080},
	}
	status := &platformv1.ServiceStatus{Allocations: []*platformv1.AllocationStatus{starting, noEndpoints, healthy, otherAgent}}

	if got := matchingHealthyAllocation(status, "", 2, 2); got.GetAllocationId() != "healthy" {
		t.Fatalf("any-agent match = %q, want healthy", got.GetAllocationId())
	}
	if got := matchingHealthyAllocation(status, "agent-b", 2, 2); got.GetAllocationId() != "other" {
		t.Fatalf("agent-b match = %q, want other", got.GetAllocationId())
	}
	if got := matchingHealthyAllocation(status, "agent-a", 3, 2); got != nil {
		t.Fatalf("spec revision 3 match = %+v, want nil", got)
	}
	if got := matchingHealthyAllocation(status, "agent-a", 2, 3); got != nil {
		t.Fatalf("rollout generation 3 match = %+v, want nil", got)
	}
	if got := matchingHealthyAllocation(status, "missing", 2, 2); got != nil {
		t.Fatalf("missing agent match = %+v, want nil", got)
	}
}

func TestAllocationEndpoints(t *testing.T) {
	dual := &platformv1.AllocationStatus{
		AllocationIpv4: "10.0.0.5", HealthyIpv4Ports: []int32{8080},
		AllocationIpv6: "fd00::5", HealthyIpv6Ports: []int32{9090},
	}
	if got, want := allocationEndpoints(dual), []string{"10.0.0.5:8080", "[fd00::5]:9090"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dual-stack endpoints = %v, want %v", got, want)
	}
	ipv4Only := &platformv1.AllocationStatus{AllocationIpv4: "10.0.0.5", HealthyIpv4Ports: []int32{8080}}
	if got, want := allocationEndpoints(ipv4Only), []string{"10.0.0.5:8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ipv4-only endpoints = %v, want %v", got, want)
	}
	if got := allocationEndpoints(&platformv1.AllocationStatus{}); got != nil {
		t.Fatalf("address without healthy ports = %v, want nil", got)
	}
	if got := allocationEndpoint(nil); got != "" {
		t.Fatalf("nil allocation endpoint = %q, want empty", got)
	}
}
