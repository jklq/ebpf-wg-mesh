package journal

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"
)

func projectionFixture() DurableState {
	name, now := "system", time.Unix(1, 0).UTC()
	return DurableState{
		ClusterID: "test", LogIndex: 1,
		Projects:       map[string]Project{"p": {ID: "p", SystemKey: &name}, "q": {ID: "q"}},
		Environments:   map[string]Environment{"e": {ID: "e", ProjectID: "p", CopiedFromEnvironmentID: &name}, "f": {ID: "f", ProjectID: "q"}},
		Services:       map[string]ServiceIntent{"s": {ID: "s", EnvironmentID: "e"}, "t": {ID: "t", EnvironmentID: "f"}},
		Revisions:      map[string]ServiceRevision{"s/1": {ServiceID: "s", SpecRevision: 1, SpecJSON: []byte(`{"runtime":{}}`), EnvDEKID: "dek", EnvCiphertext: []byte("ciphertext")}},
		Agents:         map[string]AgentRegistration{"one": {ID: "one", RuntimeCapabilities: []byte(`["linux"]`)}, "two": {ID: "two"}},
		Administration: map[string]AgentAdministration{"one": {AgentID: "one", CredentialRevokedAt: &now}},
		Deployments:    map[string]Deployment{"d": {ID: "d", ServiceID: "s", SpecRevision: 1}},
		Rollouts:       map[string]Rollout{"s/1": {ServiceID: "s", RolloutGeneration: 1, StrategyJSON: []byte(`{"maxSurge":1}`), CompletedAt: &now}},
		Assignments: map[string]Assignment{
			"a": {ID: "a", AgentID: "one", ServiceID: "s", DeploymentID: "d", DrainStartedAt: &now, DrainDeadline: &now},
			"b": {ID: "b", AgentID: "one", ServiceID: "s", DeploymentID: "d"},
			"c": {ID: "c", AgentID: "two", ServiceID: "t", DeploymentID: "d"},
		},
		Volumes: map[string]Volume{"v": {ID: "v", EnvironmentID: "e"}},
		Domains: map[string]Domain{"app.test": {Hostname: "app.test", ServiceID: "s"}},
	}
}

func advanceProjection(t *testing.T, before *Projection, state DurableState) *Projection {
	t.Helper()
	batch := Diff(before.DurableState, state)
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := before.advance(Entry{ClusterID: before.ClusterID, LogIndex: before.LogIndex + 1,
		CommandID: "command", CommandVersion: CommandVersion, CommandType: CommandType, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	// Entry storage can be reused after application; the projection owns decoded rows.
	clear(raw)
	want := projectionRelations(NewProjection(next.DurableState).indexes)
	for name, index := range projectionRelations(next.indexes) {
		if !maps.EqualFunc(index, want[name], func(a, b map[string]int) bool { return maps.Equal(a, b) }) {
			t.Fatalf("incremental %s differs from rebuilt relationships:\n got %+v\nwant %+v", name, index, want[name])
		}
	}
	return next
}

func projectionRelations(p projectionIndexes) map[string]relation {
	return map[string]relation{
		"assignmentsByAgent": p.assignmentsByAgent, "assignmentsByService": p.assignmentsByService,
		"servicesByEnvironment": p.servicesByEnvironment, "domainsByService": p.domainsByService,
		"environmentsByAgent": p.environmentsByAgent, "agentsByEnvironment": p.agentsByEnvironment,
		"environmentsByProject": p.environmentsByProject, "volumesByEnvironment": p.volumesByEnvironment,
	}
}

func projectionJSON(t *testing.T, state DurableState) string {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestProjectionOwnsImportedSnapshotAndPreservesEarlierPrefixes(t *testing.T) {
	state := projectionFixture()
	before := NewProjection(state)
	initialJSON := projectionJSON(t, before.DurableState)
	initialIndexes := NewProjection(before.DurableState).indexes
	// Maps, nested JSON, and pointer-bearing rows must all belong to the import.
	delete(state.Services, "t")
	*state.Projects["p"].SystemKey = "changed"
	*state.Environments["e"].CopiedFromEnvironmentID = "changed"
	*state.Assignments["a"].DrainDeadline = time.Unix(9, 0)
	*state.Assignments["a"].DrainStartedAt = time.Unix(10, 0)
	*state.Administration["one"].CredentialRevokedAt = time.Unix(11, 0)
	*state.Rollouts["s/1"].CompletedAt = time.Unix(12, 0)
	for _, raw := range [][]byte{state.Revisions["s/1"].SpecJSON, state.Revisions["s/1"].EnvCiphertext,
		state.Agents["one"].RuntimeCapabilities, state.Rollouts["s/1"].StrategyJSON} {
		clear(raw)
	}
	if got := projectionJSON(t, before.DurableState); got != initialJSON {
		t.Fatal("caller mutation changed the imported projection")
	}
	next := before.DurableState.Clone()
	service := next.Services["s"]
	service.EnvironmentID = "f"
	next.Services["s"] = service
	revision := next.Revisions["s/1"]
	revision.SpecJSON = []byte(`{"runtime":{"env":{"VALUE":"next"}}}`)
	next.Revisions["s/1"] = revision
	advanced := advanceProjection(t, before, next)
	if got := projectionJSON(t, before.DurableState); got != initialJSON || !reflect.DeepEqual(before.indexes, initialIndexes) {
		t.Fatal("advancing changed an earlier prefix or its relationship indexes")
	}
	copy := advanced.DurableState.Clone()
	clear(copy.Revisions["s/1"].SpecJSON)
	if string(advanced.Revisions["s/1"].SpecJSON) != `{"runtime":{"env":{"VALUE":"next"}}}` {
		t.Fatal("mutable snapshot clone shares JSON with the projection")
	}
}

func TestProjectionMembershipCountsServiceMovesAndLostAssignments(t *testing.T) {
	p := NewProjection(projectionFixture())
	if got := p.EnvironmentIDsForAgent("one"); !slices.Equal(got, []string{"e"}) || p.indexes.agentsByEnvironment["e"]["one"] != 2 {
		t.Fatalf("two assignments did not retain counted membership: %v", got)
	}
	next := p.DurableState.Clone()
	delete(next.Assignments, "a")
	p = advanceProjection(t, p, next)
	if got := p.AgentIDsForEnvironment("e"); !slices.Equal(got, []string{"one"}) || p.indexes.agentsByEnvironment["e"]["one"] != 1 {
		t.Fatalf("removing one replica removed the peer: %v", got)
	}
	// No assignment changes accompany a service move. Both directions of network
	// membership must follow the service, while the immutable old prefix survives.
	old := p
	next = p.DurableState.Clone()
	s := next.Services["s"]
	s.EnvironmentID = "f"
	next.Services["s"] = s
	p = advanceProjection(t, p, next)
	if got := p.AgentIDsForEnvironment("f"); !slices.Equal(got, []string{"one", "two"}) || len(p.AgentIDsForEnvironment("e")) != 0 {
		t.Fatalf("service move membership is wrong: e=%v f=%v", p.AgentIDsForEnvironment("e"), got)
	}
	if got := old.EnvironmentIDsForAgent("one"); !slices.Equal(got, []string{"e"}) {
		t.Fatalf("service move changed old membership: %v", got)
	}
	next = p.DurableState.Clone()
	a := next.Assignments["b"]
	a.RolloutState = "lost"
	next.Assignments["b"] = a
	p = advanceProjection(t, p, next)
	if len(p.EnvironmentIDsForAgent("one")) != 0 || !slices.Equal(p.AssignmentIDsForAgent("one"), []string{"b"}) {
		t.Fatal("lost assignment must release peer scope while retaining its product identity")
	}
	next = p.DurableState.Clone()
	a.RolloutState = "serving"
	next.Assignments["b"] = a
	p = advanceProjection(t, p, next)
	if got := p.EnvironmentIDsForAgent("one"); !slices.Equal(got, []string{"f"}) {
		t.Fatalf("restored assignment did not restore peer scope: %v", got)
	}
}

func TestProjectionPrunesEmptyGroupsAndRecreatesThemWithinOneBatch(t *testing.T) {
	p := NewProjection(projectionFixture())
	next := p.DurableState.Clone()
	// Swapping services removes and recreates each outer environment bucket in
	// one batch. Copy-on-write ownership must survive that recreation.
	s, other := next.Services["s"], next.Services["t"]
	s.EnvironmentID, other.EnvironmentID = other.EnvironmentID, s.EnvironmentID
	next.Services["s"], next.Services["t"] = s, other
	p = advanceProjection(t, p, next)
	if got := p.ServiceIDsForEnvironment("e"); !slices.Equal(got, []string{"t"}) {
		t.Fatalf("recreated service bucket = %v", got)
	}
	p = advanceProjection(t, p, DurableState{ClusterID: p.ClusterID})
	for name, index := range projectionRelations(p.indexes) {
		if len(index) != 0 {
			t.Fatalf("deletion retained empty %s groups: %v", name, index)
		}
	}
	// Restoration and compaction recovery import the current normalized state.
	restored := NewProjection(projectionFixture())
	if got := restored.AgentIDsForEnvironment("e"); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("rebuilt projection lost restored relationships: %v", got)
	}
}
