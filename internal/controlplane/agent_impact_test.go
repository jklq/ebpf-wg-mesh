package controlplane

import (
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestAgentDeltaRequiresClusterFanout(t *testing.T) {
	base := journal.DurableState{
		Agents: map[string]journal.AgentRegistration{
			"node-1": peerAgent("node-1"),
		},
		Administration: map[string]journal.AgentAdministration{
			"node-1": {AgentID: "node-1", LifecycleState: "active"},
		},
	}

	t.Run("session and timestamp do not fan out", func(t *testing.T) {
		after := peerAgent("node-1")
		after.SessionIncarnation = 2
		after.UpdatedAt = time.Now().UTC()
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if agentDeltaRequiresClusterFanout(base, batch) {
			t.Fatal("reconnect hello fanned out")
		}
		if ids := selfAgentIDs(base, batch); len(ids) != 0 {
			t.Fatalf("self bump %v", ids)
		}
	})

	t.Run("peer field change fans out", func(t *testing.T) {
		after := peerAgent("node-1")
		after.AdvertiseAddr = "fd00:30::99"
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if !agentDeltaRequiresClusterFanout(base, batch) {
			t.Fatal("advertise_addr change did not fan out")
		}
	})

	t.Run("retire fans out", func(t *testing.T) {
		admin := journal.AgentAdministration{AgentID: "node-1", LifecycleState: "retired"}
		batch := journal.Batch{Administration: []journal.Change[journal.AgentAdministration]{{Key: "node-1", Value: &admin}}}
		if !agentDeltaRequiresClusterFanout(base, batch) {
			t.Fatal("retire did not fan out")
		}
	})

	t.Run("empty enroll does not fan out", func(t *testing.T) {
		empty := journal.AgentRegistration{ID: "node-2", Name: "node-2"}
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-2", Value: &empty}}}
		if agentDeltaRequiresClusterFanout(base, batch) {
			t.Fatal("empty enroll fanned out")
		}
	})

	t.Run("first hello with keys fans out", func(t *testing.T) {
		emptyBase := journal.DurableState{
			Agents: map[string]journal.AgentRegistration{
				"node-2": {ID: "node-2", Name: "node-2"},
			},
			Administration: map[string]journal.AgentAdministration{
				"node-2": {AgentID: "node-2", LifecycleState: "enrolling"},
			},
		}
		after := peerAgent("node-2")
		active := journal.AgentAdministration{AgentID: "node-2", LifecycleState: "active"}
		batch := journal.Batch{
			Agents:         []journal.Change[journal.AgentRegistration]{{Key: "node-2", Value: &after}},
			Administration: []journal.Change[journal.AgentAdministration]{{Key: "node-2", Value: &active}},
		}
		if !agentDeltaRequiresClusterFanout(emptyBase, batch) {
			t.Fatal("first hello did not fan out")
		}
	})

	t.Run("wireguard ipv6 only bumps self", func(t *testing.T) {
		after := peerAgent("node-1")
		after.WireguardIPv6 = "fd00:44::ff"
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if agentDeltaRequiresClusterFanout(base, batch) {
			t.Fatal("own wireguard address fanned out")
		}
		if ids := selfAgentIDs(base, batch); len(ids) != 1 || ids[0] != "node-1" {
			t.Fatalf("self bump %v", ids)
		}
	})
}

func TestServiceAndVolumeEnvironmentIDs(t *testing.T) {
	base := journal.DurableState{
		Services: map[string]journal.ServiceIntent{
			"svc-a": {ID: "svc-a", EnvironmentID: "env-1"},
		},
		Volumes: map[string]journal.Volume{
			"vol-a": {ID: "vol-a", EnvironmentID: "env-1"},
		},
	}
	renamed := journal.ServiceIntent{ID: "svc-a", EnvironmentID: "env-1", Name: "renamed"}
	batch := journal.Batch{
		Services: []journal.Change[journal.ServiceIntent]{{Key: "svc-a", Value: &renamed}},
		Volumes:  []journal.Change[journal.Volume]{{Key: "vol-a"}},
	}
	if got := uniqueStrings(serviceEnvironmentIDs(base, batch, []string{"svc-a"})); len(got) != 1 || got[0] != "env-1" {
		t.Fatalf("service envs %v", got)
	}
	if got := uniqueStrings(volumeEnvironmentIDs(base, batch)); len(got) != 1 || got[0] != "env-1" {
		t.Fatalf("volume envs %v", got)
	}
}

func peerAgent(id string) journal.AgentRegistration {
	return journal.AgentRegistration{
		ID:                  id,
		Name:                id,
		AdvertiseAddr:       "fd00:30::10",
		WorkloadIPv4Subnet:  "10.200.1.0/24",
		WorkloadIPv6Subnet:  "fd00:200:1::/64",
		WireguardPublicKey:  "test-public-key",
		WireguardListenPort: 51820,
		WireguardIPv6:       "fd00:44::10",
	}
}
