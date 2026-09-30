package controlplane

import (
	"context"
	"slices"
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

func TestAffectedAgentsUseBothIndexedPrefixes(t *testing.T) {
	state := journal.DurableState{
		Projects:       map[string]journal.Project{"p": {ID: "p"}, "q": {ID: "q"}},
		Environments:   map[string]journal.Environment{"e": {ID: "e", ProjectID: "p"}, "f": {ID: "f", ProjectID: "q"}},
		Services:       map[string]journal.ServiceIntent{"s": {ID: "s", EnvironmentID: "e"}, "sibling": {ID: "sibling", EnvironmentID: "e"}, "foreign": {ID: "foreign", EnvironmentID: "f"}},
		Agents:         map[string]journal.AgentRegistration{"one": peerAgent("one"), "two": peerAgent("two"), "three": peerAgent("three")},
		Administration: map[string]journal.AgentAdministration{"one": {AgentID: "one", LifecycleState: "active"}},
		Deployments:    map[string]journal.Deployment{"d": {ID: "d"}},
		Assignments:    map[string]journal.Assignment{"a": {ID: "a", AgentID: "one", ServiceID: "s", DeploymentID: "d"}, "b": {ID: "b", AgentID: "two", ServiceID: "sibling", DeploymentID: "d"}, "c": {ID: "c", AgentID: "three", ServiceID: "foreign", DeploymentID: "d"}},
		Domains:        map[string]journal.Domain{"example.test": {Hostname: "example.test", ServiceID: "s"}},
		Volumes:        map[string]journal.Volume{"v": {ID: "v", EnvironmentID: "e"}},
	}
	before := journal.NewProjection(state)
	tests := []struct {
		name   string
		change func(*journal.DurableState)
		want   []string
	}{
		{"reconnect", func(s *journal.DurableState) {
			a := s.Agents["one"]
			a.SessionIncarnation++
			a.UpdatedAt = time.Now()
			s.Agents["one"] = a
		}, nil},
		{"own listen port", func(s *journal.DurableState) { a := s.Agents["one"]; a.WireguardListenPort++; s.Agents["one"] = a }, []string{"one"}},
		{"peer endpoint", func(s *journal.DurableState) {
			a := s.Agents["one"]
			a.WireguardEndpoint = "192.0.2.1:51820"
			s.Agents["one"] = a
		}, []string{"one", "two"}},
		{"retire peer", func(s *journal.DurableState) {
			a := s.Administration["one"]
			a.LifecycleState = "retired"
			s.Administration["one"] = a
		}, []string{"one", "two"}},
		{"revoke peer", func(s *journal.DurableState) {
			a := s.Administration["one"]
			now := time.Now()
			a.CredentialRevokedAt = &now
			s.Administration["one"] = a
		}, []string{"one", "two"}},
		{"volume removal", func(s *journal.DurableState) { delete(s.Volumes, "v") }, []string{"one", "two"}},
		{"service move", func(s *journal.DurableState) { a := s.Services["s"]; a.EnvironmentID = "f"; s.Services["s"] = a }, []string{"one", "three", "two"}},
		{"domain move", func(s *journal.DurableState) {
			a := s.Domains["example.test"]
			a.ServiceID = "foreign"
			s.Domains["example.test"] = a
		}, []string{"one", "three"}},
		{"project visibility removal", func(s *journal.DurableState) {
			delete(s.Projects, "p")
			delete(s.Environments, "e")
			delete(s.Services, "s")
			delete(s.Services, "sibling")
			delete(s.Assignments, "a")
			delete(s.Assignments, "b")
		}, []string{"one", "two"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := state.Clone()
			test.change(&next)
			batch := journal.Diff(state, next)
			after, err := before.Preview(batch)
			if err != nil {
				t.Fatal(err)
			}
			if got := affectedAgentIDs(before, after, batch); !slices.Equal(got, test.want) {
				t.Fatalf("affected=%v want=%v", got, test.want)
			}
		})
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
