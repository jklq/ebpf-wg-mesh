package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"time"
)

const CommandVersion = 1
const CommandType = "durable_batch"

type Entry struct {
	ClusterID        string
	LogIndex         int64
	CommandID        string
	CommandVersion   int
	CommandType      string
	Payload          json.RawMessage
	AuthorizingEpoch *int64
	CreatedAt        time.Time
}

// Apply is deterministic and atomic. Authority is checked at append, never
// during replay: a committed decision outlives the lease that authorized it.
func (s DurableState) Apply(entry Entry) (DurableState, error) {
	if entry.ClusterID != s.ClusterID || entry.LogIndex != s.LogIndex+1 {
		return s, fmt.Errorf("journal prefix mismatch: cluster %q index %d, expected %q index %d", entry.ClusterID, entry.LogIndex, s.ClusterID, s.LogIndex+1)
	}
	if entry.CommandID == "" || entry.CommandVersion != CommandVersion || entry.CommandType != CommandType {
		return s, fmt.Errorf("unsupported journal command %q version %d", entry.CommandType, entry.CommandVersion)
	}
	var batch Batch
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		return s, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return s, fmt.Errorf("trailing journal payload")
	}
	if batch.BaseIndex != s.LogIndex {
		return s, fmt.Errorf("stale durable precondition: %d != %d", batch.BaseIndex, s.LogIndex)
	}
	next, err := s.applyBatch(batch)
	if err != nil {
		return s, err
	}
	next.LogIndex = entry.LogIndex
	return next, nil
}

func applyChanges[T any](previous map[string]T, changes []Change[T]) (map[string]T, error) {
	next := maps.Clone(previous)
	if next == nil {
		next = make(map[string]T)
	}
	seen := make(map[string]bool, len(changes))
	for _, change := range changes {
		if change.Key == "" || seen[change.Key] {
			return nil, fmt.Errorf("empty or repeated durable key %q", change.Key)
		}
		seen[change.Key] = true
		if change.Value == nil {
			delete(next, change.Key)
		} else {
			next[change.Key] = *change.Value
		}
	}
	return next, nil
}

func changes[T any](before, after map[string]T) []Change[T] {
	var result []Change[T]
	for key, value := range after {
		if previous, exists := before[key]; !exists || !reflect.DeepEqual(previous, value) {
			result = append(result, Change[T]{Key: key, Value: &value})
		}
	}
	for key := range before {
		if _, exists := after[key]; !exists {
			result = append(result, Change[T]{Key: key})
		}
	}
	slices.SortFunc(result, func(a, b Change[T]) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	return result
}

func (s DurableState) validateReservations() error {
	addresses := make(map[string]string)
	for id, assignment := range s.Assignments {
		if assignment.ID != id || assignment.AgentID == "" || assignment.ServiceID == "" || assignment.DeploymentID == "" {
			return fmt.Errorf("incomplete assignment %q", id)
		}
		if _, ok := s.Agents[assignment.AgentID]; !ok {
			return fmt.Errorf("assignment %q has no agent", id)
		}
		if _, ok := s.Services[assignment.ServiceID]; !ok {
			return fmt.Errorf("assignment %q has no service", id)
		}
		if _, ok := s.Deployments[assignment.DeploymentID]; !ok {
			return fmt.Errorf("assignment %q has no deployment", id)
		}
		for _, address := range []string{assignment.AllocationIPv4, assignment.AllocationIPv6} {
			if address == "" {
				continue
			}
			if owner, exists := addresses[address]; exists {
				return fmt.Errorf("address %s reserved by both %s and %s", address, owner, id)
			}
			addresses[address] = id
		}
	}
	return nil
}

func (s DurableState) applyBatch(batch Batch) (DurableState, error) {
	for _, change := range batch.Assignments {
		if before, exists := s.Assignments[change.Key]; exists && change.Value != nil &&
			(before.AgentID != change.Value.AgentID || before.ServiceID != change.Value.ServiceID) {
			return s, fmt.Errorf("assignment %q cannot change owner", change.Key)
		}
	}
	next := s
	var err error
	next.Projects, err = applyChanges(s.Projects, batch.Projects)
	if err != nil {
		return s, err
	}
	next.Services, err = applyChanges(s.Services, batch.Services)
	if err != nil {
		return s, err
	}
	next.Revisions, err = applyChanges(s.Revisions, batch.Revisions)
	if err != nil {
		return s, err
	}
	next.Assignments, err = applyChanges(s.Assignments, batch.Assignments)
	if err != nil {
		return s, err
	}
	next.Rollouts, err = applyChanges(s.Rollouts, batch.Rollouts)
	if err != nil {
		return s, err
	}
	next.Deployments, err = applyChanges(s.Deployments, batch.Deployments)
	if err != nil {
		return s, err
	}
	next.Agents, err = applyChanges(s.Agents, batch.Agents)
	if err != nil {
		return s, err
	}
	next.Administration, err = applyChanges(s.Administration, batch.Administration)
	if err != nil {
		return s, err
	}
	next.Environments, err = applyChanges(s.Environments, batch.Environments)
	if err != nil {
		return s, err
	}
	next.Volumes, err = applyChanges(s.Volumes, batch.Volumes)
	if err != nil {
		return s, err
	}
	next.Domains, err = applyChanges(s.Domains, batch.Domains)
	if err != nil {
		return s, err
	}
	if err := next.validateReservations(); err != nil {
		return s, err
	}
	return next, nil
}

func (b Batch) requiresAuthority() bool {
	if len(b.Assignments) != 0 || len(b.Rollouts) != 0 {
		return true
	}
	for _, change := range b.Deployments {
		if change.Value != nil && change.Value.State != "staged" {
			return true
		}
	}
	return false
}

func Diff(before, after DurableState) Batch {
	return Batch{BaseIndex: before.LogIndex,
		Projects:       changes(before.Projects, after.Projects),
		Services:       changes(before.Services, after.Services),
		Revisions:      changes(before.Revisions, after.Revisions),
		Assignments:    changes(before.Assignments, after.Assignments),
		Rollouts:       changes(before.Rollouts, after.Rollouts),
		Deployments:    changes(before.Deployments, after.Deployments),
		Agents:         changes(before.Agents, after.Agents),
		Administration: changes(before.Administration, after.Administration),
		Environments:   changes(before.Environments, after.Environments),
		Volumes:        changes(before.Volumes, after.Volumes),
		Domains:        changes(before.Domains, after.Domains),
	}
}
