package journal

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func testEntry(t *testing.T, index int64, batch Batch) Entry {
	t.Helper()
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	epoch := int64(1)
	return Entry{ClusterID: "test", LogIndex: index, CommandID: "command", CommandVersion: CommandVersion, CommandType: CommandType, Payload: payload, AuthorizingEpoch: &epoch}
}

func assignedBatch() Batch {
	return Batch{
		Services:    []Change[ServiceIntent]{{Key: "service", Value: &ServiceIntent{ID: "service", CurrentSpecRevision: 1}}},
		Agents:      []Change[AgentRegistration]{{Key: "agent", Value: &AgentRegistration{ID: "agent"}}},
		Deployments: []Change[Deployment]{{Key: "deployment", Value: &Deployment{ID: "deployment", ServiceID: "service"}}},
		Assignments: []Change[Assignment]{{Key: "allocation", Value: &Assignment{ID: "allocation", AgentID: "agent", ServiceID: "service", DeploymentID: "deployment", AllocationIPv4: "10.0.0.1", AllocationIPv6: "fd00::1", Intent: "run"}}},
	}
}

func TestReplayDurableStateAndDeadline(t *testing.T) {
	first := testEntry(t, 1, assignedBatch())
	deadline := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	draining := *assignedBatch().Assignments[0].Value
	draining.Intent, draining.RolloutState, draining.DrainDeadline = "drain", "draining", &deadline
	second := testEntry(t, 2, Batch{BaseIndex: 1, Assignments: []Change[Assignment]{{Key: draining.ID, Value: &draining}}})
	state := DurableState{ClusterID: "test"}
	var err error
	for _, entry := range []Entry{first, second} {
		state, err = state.Apply(entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	replayed := DurableState{ClusterID: "test"}
	for _, entry := range []Entry{first, second} {
		replayed, err = replayed.Apply(entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(state, replayed) || !state.Assignments.Get("allocation").DrainDeadline.Equal(deadline) {
		t.Fatalf("replay changed durable state: %+v", replayed)
	}
}

func TestBillingRestrictionDiffApplyRoundtrip(t *testing.T) {
	since := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	before := DurableState{ClusterID: "test", Restrictions: map[string]BillingRestriction{}}
	after := before.Clone()
	after.Projects = map[string]Project{"project": {ID: "project", WorkspaceID: "workspace-1"}}
	after.Restrictions = map[string]BillingRestriction{
		"workspace-1": {WorkspaceID: "workspace-1", AccountID: "account-1", Restricted: true, DryRun: true, Reason: "budget stop threshold reached", Since: &since, UpdatedAt: since},
	}
	batch := Diff(before, after)
	if len(batch.Restrictions) != 1 || len(batch.Projects) != 1 {
		t.Fatalf("diff = %+v", batch)
	}
	applied, err := before.Apply(testEntry(t, 1, batch))
	if err != nil {
		t.Fatal(err)
	}
	if got := applied.Restrictions["workspace-1"]; !got.Restricted || !got.DryRun || got.Reason == "" {
		t.Fatalf("restriction after apply = %+v", got)
	}
	if applied.Projects["project"].WorkspaceID != "workspace-1" {
		t.Fatalf("project workspace after apply = %+v", applied.Projects["project"])
	}
	cleared := Diff(applied, DurableState{ClusterID: "test", LogIndex: 1, Restrictions: map[string]BillingRestriction{}, Projects: map[string]Project{}})
	applied, err = applied.Apply(testEntry(t, 2, cleared))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Restrictions) != 0 {
		t.Fatalf("restrictions after clear = %+v", applied.Restrictions)
	}
}

func TestApplyRejectsInvalidBatchAtomically(t *testing.T) {
	initial := DurableState{ClusterID: "test"}
	state, err := initial.Apply(testEntry(t, 1, assignedBatch()))
	if err != nil {
		t.Fatal(err)
	}
	conflicting := *assignedBatch().Assignments[0].Value
	conflicting.ID = "another"
	entry := testEntry(t, 2, Batch{BaseIndex: 1,
		Services:    []Change[ServiceIntent]{{Key: "service", Value: &ServiceIntent{ID: "service", CurrentSpecRevision: 2}}},
		Assignments: []Change[Assignment]{{Key: conflicting.ID, Value: &conflicting}},
	})
	for name, mutate := range map[string]func(*Entry){
		"conflicting reservation": func(*Entry) {},
		"gap":                     func(e *Entry) { e.LogIndex = 3 },
		"foreign cluster":         func(e *Entry) { e.ClusterID = "other" },
		"unknown version":         func(e *Entry) { e.CommandVersion++ },
		"unknown command":         func(e *Entry) { e.CommandType = "future" },
		"stale precondition":      func(e *Entry) { e.Payload = json.RawMessage(`{"base_index":0}`) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := entry
			mutate(&candidate)
			result, err := state.Apply(candidate)
			if err == nil {
				t.Fatal("invalid entry accepted")
			}
			if !reflect.DeepEqual(result, state) || state.Services["service"].CurrentSpecRevision != 1 {
				t.Fatal("failed batch changed state")
			}
		})
	}
}

func TestProductJSONCanonicalizationDoesNotRepeatCommands(t *testing.T) {
	before := DurableState{}
	if err := decodeRecord(&before.Revisions, "service/1", []byte(`{"service_id": "service", "spec_revision": 1, "spec_json": {"image": "web"}}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var replayed DurableState
	if err := json.Unmarshal(raw, &replayed); err != nil {
		t.Fatal(err)
	}
	if batch := Diff(replayed, before); len(batch.Revisions) != 0 {
		t.Fatalf("unchanged spec was journaled again: %+v", batch)
	}
}
