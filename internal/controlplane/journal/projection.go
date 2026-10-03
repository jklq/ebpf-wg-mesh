package journal

import (
	"maps"
	"slices"
)

// Projection is the immutable product view of one committed journal prefix.
// Its rows and returned state are borrowed: readers must never mutate them.
// Advancing the prefix shares unchanged tables and index buckets. Store and
// Live hold the same projection; live observations remain separately owned.
type Projection struct {
	DurableState
	indexes projectionIndexes
}

// NewProjection imports an owned snapshot. Normal journal replay uses advance,
// which copies only the tables and relationship buckets touched by a batch.
func NewProjection(state DurableState) *Projection {
	return newProjection(state.Clone())
}

func newProjection(state DurableState) *Projection {
	p := &Projection{DurableState: state}
	p.indexes = updateIndexes(projectionIndexes{}, DurableState{}, state, Diff(DurableState{}, state))
	return p
}

func (p *Projection) advance(entry Entry) (*Projection, Batch, error) {
	state, batch, err := p.DurableState.applyEntry(entry)
	if err != nil {
		return p, Batch{}, err
	}
	return &Projection{DurableState: state, indexes: updateIndexes(p.indexes, p.DurableState, state, batch)}, batch, nil
}

// Preview applies the resolved SQL effects before their agent revision bumps.
// It never publishes attempted state or advances the committed prefix.
func (p *Projection) Preview(batch Batch) (*Projection, error) {
	state, err := p.DurableState.applyBatch(batch)
	if err != nil {
		return nil, err
	}
	return &Projection{DurableState: state, indexes: updateIndexes(p.indexes, p.DurableState, state, batch)}, nil
}

func (p *Projection) AssignmentIDsForAgent(id string) []string {
	return p.indexes.assignmentsByAgent.keys(id)
}
func (p *Projection) AssignmentIDsForService(id string) []string {
	return p.indexes.assignmentsByService.keys(id)
}
func (p *Projection) ServiceIDsForEnvironment(id string) []string {
	return p.indexes.servicesByEnvironment.keys(id)
}
func (p *Projection) DomainHostnamesForService(id string) []string {
	return p.indexes.domainsByService.keys(id)
}
func (p *Projection) EnvironmentIDsForAgent(id string) []string {
	return p.indexes.environmentsByAgent.keys(id)
}
func (p *Projection) AgentIDsForEnvironment(id string) []string {
	return p.indexes.agentsByEnvironment.keys(id)
}
func (p *Projection) EnvironmentIDsForProject(id string) []string {
	return p.indexes.environmentsByProject.keys(id)
}
func (p *Projection) VolumeIDsForEnvironment(id string) []string {
	return p.indexes.volumesByEnvironment.keys(id)
}

// VolumeIDsForAgent lists live volumes pinned to an agent.
func (p *Projection) VolumeIDsForAgent(id string) []string {
	return p.indexes.volumesByAgent.keys(id)
}

// DestructionIDsForAgent lists volumes an agent has been told to destroy.
func (p *Projection) DestructionIDsForAgent(id string) []string {
	return p.indexes.destructionsByAgent.keys(id)
}

// A membership count retains environment peers until their last non-lost
// assignment disappears. The same relation machinery handles ordinary sets.
type relation map[string]map[string]int

func (r relation) keys(key string) []string {
	keys := slices.Collect(maps.Keys(r[key]))
	slices.Sort(keys)
	return keys
}

type relationEditor struct {
	next  relation
	owned map[string]bool
}

func editRelation(previous relation) relationEditor { return relationEditor{next: previous} }

func (e *relationEditor) add(key, member string, delta int) {
	if key == "" || member == "" {
		return
	}
	if e.owned == nil {
		e.next = maps.Clone(e.next)
		if e.next == nil {
			e.next = make(relation)
		}
		e.owned = make(map[string]bool)
	}
	if !e.owned[key] {
		e.next[key] = maps.Clone(e.next[key])
		if e.next[key] == nil {
			e.next[key] = make(map[string]int)
		}
		e.owned[key] = true
	}
	n := e.next[key][member] + delta
	if n > 0 {
		if e.next[key] == nil {
			e.next[key] = make(map[string]int)
		}
		e.next[key][member] = n
	} else {
		delete(e.next[key], member)
		if len(e.next[key]) == 0 {
			delete(e.next, key)
		}
	}
}

type projectionIndexes struct {
	assignmentsByAgent, assignmentsByService    relation
	servicesByEnvironment, domainsByService     relation
	environmentsByAgent, agentsByEnvironment    relation
	environmentsByProject, volumesByEnvironment relation
	volumesByAgent, destructionsByAgent         relation
}

func updateIndexes(previous projectionIndexes, before, after DurableState, batch Batch) projectionIndexes {
	assignmentsByAgent, assignmentsByService := editRelation(previous.assignmentsByAgent), editRelation(previous.assignmentsByService)
	environmentsByAgent, agentsByEnvironment := editRelation(previous.environmentsByAgent), editRelation(previous.agentsByEnvironment)
	servicesByEnvironment, domainsByService := editRelation(previous.servicesByEnvironment), editRelation(previous.domainsByService)
	environmentsByProject, volumesByEnvironment := editRelation(previous.environmentsByProject), editRelation(previous.volumesByEnvironment)
	volumesByAgent, destructionsByAgent := editRelation(previous.volumesByAgent), editRelation(previous.destructionsByAgent)
	for _, c := range batch.Services {
		old, next := before.Services[c.Key], after.Services[c.Key]
		if old.EnvironmentID != next.EnvironmentID {
			servicesByEnvironment.add(old.EnvironmentID, c.Key, -1)
			servicesByEnvironment.add(next.EnvironmentID, c.Key, 1)
		}
	}
	for _, c := range batch.Domains {
		old, next := before.Domains[c.Key], after.Domains[c.Key]
		if old.ServiceID != next.ServiceID {
			domainsByService.add(old.ServiceID, c.Key, -1)
			domainsByService.add(next.ServiceID, c.Key, 1)
		}
	}
	for _, c := range batch.Environments {
		old, next := before.Environments[c.Key], after.Environments[c.Key]
		if old.ProjectID != next.ProjectID {
			environmentsByProject.add(old.ProjectID, c.Key, -1)
			environmentsByProject.add(next.ProjectID, c.Key, 1)
		}
	}
	for _, c := range batch.Volumes {
		old, next := before.Volumes[c.Key], after.Volumes[c.Key]
		if old.EnvironmentID != next.EnvironmentID {
			volumesByEnvironment.add(old.EnvironmentID, c.Key, -1)
			volumesByEnvironment.add(next.EnvironmentID, c.Key, 1)
		}
		if old.AgentID != next.AgentID {
			volumesByAgent.add(old.AgentID, c.Key, -1)
			volumesByAgent.add(next.AgentID, c.Key, 1)
		}
	}
	for _, c := range batch.Destructions {
		old, next := before.Destructions[c.Key], after.Destructions[c.Key]
		if old.AgentID != next.AgentID {
			destructionsByAgent.add(old.AgentID, c.Key, -1)
			destructionsByAgent.add(next.AgentID, c.Key, 1)
		}
	}
	changedAssignments := make(map[string]struct{}, len(batch.Assignments))
	for _, c := range batch.Assignments {
		changedAssignments[c.Key] = struct{}{}
	}
	// Moving/removing a service also changes the environment membership of
	// its assignments, even when the assignments themselves have not changed.
	for _, c := range batch.Services {
		if before.Services[c.Key].EnvironmentID != after.Services[c.Key].EnvironmentID {
			for id := range previous.assignmentsByService[c.Key] {
				changedAssignments[id] = struct{}{}
			}
		}
	}
	for id := range changedAssignments {
		old, next := before.Assignments[id], after.Assignments[id]
		if old.AgentID != next.AgentID {
			assignmentsByAgent.add(old.AgentID, id, -1)
			assignmentsByAgent.add(next.AgentID, id, 1)
		}
		if old.ServiceID != next.ServiceID {
			assignmentsByService.add(old.ServiceID, id, -1)
			assignmentsByService.add(next.ServiceID, id, 1)
		}
		oldEnv, nextEnv := "", ""
		if old.RolloutState != "lost" {
			oldEnv = before.Services[old.ServiceID].EnvironmentID
		}
		if next.RolloutState != "lost" {
			nextEnv = after.Services[next.ServiceID].EnvironmentID
		}
		if old.AgentID != next.AgentID || oldEnv != nextEnv {
			environmentsByAgent.add(old.AgentID, oldEnv, -1)
			agentsByEnvironment.add(oldEnv, old.AgentID, -1)
			environmentsByAgent.add(next.AgentID, nextEnv, 1)
			agentsByEnvironment.add(nextEnv, next.AgentID, 1)
		}
	}
	return projectionIndexes{assignmentsByAgent.next, assignmentsByService.next, servicesByEnvironment.next, domainsByService.next,
		environmentsByAgent.next, agentsByEnvironment.next, environmentsByProject.next, volumesByEnvironment.next,
		volumesByAgent.next, destructionsByAgent.next}
}
