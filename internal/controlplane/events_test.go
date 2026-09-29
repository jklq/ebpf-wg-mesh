package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
)

type memoryGlobalRevision struct {
	mu       sync.Mutex
	revision int64
}

func newMemoryGlobalRevision() *memoryGlobalRevision {
	return &memoryGlobalRevision{revision: initialEnvironmentRevision}
}

func (s *memoryGlobalRevision) currentGlobalRevision(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision, nil
}

func (s *memoryGlobalRevision) advance() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	return s.revision
}

func TestPlatformEventsWaitForGlobalRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newMemoryGlobalRevision()
	events := newPlatformEvents(store, 5*time.Millisecond)
	initial, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		index   int64
		changed bool
		err     error
	}, 1)
	go func() {
		index, changed, waitErr := events.Wait(ctx, initial, time.Second)
		result <- struct {
			index   int64
			changed bool
			err     error
		}{index: index, changed: changed, err: waitErr}
	}()

	select {
	case <-result:
		t.Fatal("wait returned before the revision advanced")
	case <-time.After(20 * time.Millisecond):
	}

	want := store.advance()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.changed || got.index != want {
			t.Fatalf("wait result = %+v, want changed index %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("revision advance did not wake blocking wait")
	}
}

func TestPlatformEventsTimeoutReturnsNotModified(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := newPlatformEvents(newMemoryGlobalRevision(), 5*time.Millisecond)
	initial, err := events.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index, changed, err := events.Wait(ctx, initial, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if changed || index != initial {
		t.Fatalf("timeout = (%d, %v), want (%d, false)", index, changed, initial)
	}
}

func TestPeerChangedAgentIDs(t *testing.T) {
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
		if len(peerChangedAgentIDs(base, batch)) > 0 {
			t.Fatal("reconnect hello fanned out")
		}
		if ids := selfAgentIDs(base, batch); len(ids) != 0 {
			t.Fatalf("self bump %v", ids)
		}
	})

	t.Run("legacy advertise address does not fan out", func(t *testing.T) {
		after := peerAgent("node-1")
		after.AdvertiseAddr = "fd00:30::99"
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(base, batch)) > 0 {
			t.Fatal("unused advertise_addr change fanned out")
		}
	})

	t.Run("retire fans out", func(t *testing.T) {
		admin := journal.AgentAdministration{AgentID: "node-1", LifecycleState: "retired"}
		batch := journal.Batch{Administration: []journal.Change[journal.AgentAdministration]{{Key: "node-1", Value: &admin}}}
		if len(peerChangedAgentIDs(base, batch)) == 0 {
			t.Fatal("retire did not fan out")
		}
	})

	t.Run("empty enroll does not fan out", func(t *testing.T) {
		empty := journal.AgentRegistration{ID: "node-2", Name: "node-2"}
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-2", Value: &empty}}}
		if len(peerChangedAgentIDs(base, batch)) > 0 {
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
		if len(peerChangedAgentIDs(emptyBase, batch)) == 0 {
			t.Fatal("first hello did not fan out")
		}
	})

	t.Run("wireguard ipv6 only bumps self", func(t *testing.T) {
		after := peerAgent("node-1")
		after.WireguardIPv6 = "fd00:44::ff"
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(base, batch)) > 0 {
			t.Fatal("own wireguard address fanned out")
		}
		if ids := selfAgentIDs(base, batch); len(ids) != 1 || ids[0] != "node-1" {
			t.Fatalf("self bump %v", ids)
		}
	})

	t.Run("wireguard listen port only bumps self", func(t *testing.T) {
		after := peerAgent("node-1")
		after.WireguardListenPort = 51821
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(base, batch)) > 0 {
			t.Fatal("local WireGuard listen port fanned out")
		}
		if ids := selfAgentIDs(base, batch); len(ids) != 1 || ids[0] != "node-1" {
			t.Fatalf("self bump %v", ids)
		}
	})

	t.Run("wireguard endpoint fans out", func(t *testing.T) {
		after := peerAgent("node-1")
		after.WireguardEndpoint = "192.0.2.10:51820"
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(base, batch)) == 0 {
			t.Fatal("WireGuard endpoint change did not fan out")
		}
	})

	t.Run("wireguard endpoint cleared fans out", func(t *testing.T) {
		after := peerAgent("node-1")
		after.WireguardEndpoint = ""
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(base, batch)) == 0 {
			t.Fatal("WireGuard endpoint removal did not fan out")
		}
	})

	t.Run("wireguard endpoint set from empty fans out", func(t *testing.T) {
		emptyEndpoint := peerAgent("node-1")
		emptyEndpoint.WireguardEndpoint = ""
		emptyBase := journal.DurableState{
			Agents:         map[string]journal.AgentRegistration{"node-1": emptyEndpoint},
			Administration: map[string]journal.AgentAdministration{"node-1": {AgentID: "node-1", LifecycleState: "active"}},
		}
		after := peerAgent("node-1")
		batch := journal.Batch{Agents: []journal.Change[journal.AgentRegistration]{{Key: "node-1", Value: &after}}}
		if len(peerChangedAgentIDs(emptyBase, batch)) == 0 {
			t.Fatal("WireGuard endpoint set from empty did not fan out")
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
		WireguardEndpoint:   "[fd00:30::10]:51820",
		WireguardIPv6:       "fd00:44::10",
	}
}
