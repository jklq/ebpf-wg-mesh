package delivery

import (
	"slices"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestSharedEnvironmentMembership(t *testing.T) {
	live := &Live{durable: journal.DurableState{
		Agents:         make(map[string]journal.AgentRegistration),
		Administration: make(map[string]journal.AgentAdministration),
		Environments: map[string]journal.Environment{
			"a": {ID: "a", NetworkIdentity: 1},
			"b": {ID: "b", NetworkIdentity: 2},
		},
		Services: map[string]journal.ServiceIntent{
			"a": {ID: "a", EnvironmentID: "a"},
			"b": {ID: "b", EnvironmentID: "b"},
		},
		Assignments: map[string]journal.Assignment{
			"1a":       {ID: "1a", AgentID: "1", ServiceID: "a"},
			"2a":       {ID: "2a", AgentID: "2", ServiceID: "a"},
			"2a-extra": {ID: "2a-extra", AgentID: "2", ServiceID: "a"},
			"2b":       {ID: "2b", AgentID: "2", ServiceID: "b"},
			"3b":       {ID: "3b", AgentID: "3", ServiceID: "b"},
		},
	}}
	for _, id := range []string{"1", "2", "3"} {
		live.durable.Agents[id] = journal.AgentRegistration{
			ID: id, WireguardPublicKey: "key-" + id, WireguardEndpoint: "192.0.2.1:51820",
			WorkloadIPv4Subnet: "10.200.0.0/24", WorkloadIPv6Subnet: "fd00:200::/64",
		}
	}
	check := func(wantPeers []string, wantIdentities int) {
		t.Helper()
		live.rebuildIndexesLocked()
		cfg, err := assignedNodeConfigForAgent(live.durable, cloneLiveIndexes(live.indexes), config.ControlPlaneMeshConfig{}, "1")
		if err != nil {
			t.Fatal(err)
		}
		var peers []string
		for _, peer := range cfg.Peers {
			peers = append(peers, peer.AgentId)
		}
		if !slices.Equal(peers, wantPeers) || len(cfg.WorkloadIdentities) != wantIdentities {
			t.Fatalf("peers=%v identities=%d, want %v/%d", peers, len(cfg.WorkloadIdentities), wantPeers, wantIdentities)
		}
		for _, identity := range cfg.WorkloadIdentities {
			if identity.EnvironmentId != "a" {
				t.Fatalf("unhosted environment identity: %v", identity)
			}
		}
	}
	check([]string{"2"}, 3)
	if !slices.Equal(live.indexes.environmentsByAgent["2"], []string{"a", "b"}) ||
		!slices.Equal(live.indexes.agentsByEnvironment["a"], []string{"1", "2"}) {
		t.Fatal("membership indexes contain duplicates")
	}
	now := time.Now()
	for _, admin := range []journal.AgentAdministration{
		{LifecycleState: "retired"},
		{CredentialRevokedAt: &now},
	} {
		live.durable.Administration["2"] = admin
		check(nil, 3)
	}
	delete(live.durable.Administration, "2")
	delete(live.durable.Assignments, "2a-extra")
	check([]string{"2"}, 2)
	assignment := live.durable.Assignments["2a"]
	assignment.RolloutState = AllocationRolloutLost
	live.durable.Assignments["2a"] = assignment
	check(nil, 1)
	delete(live.durable.Assignments, "1a")
	check(nil, 0)
}
