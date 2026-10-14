package journal

import (
	"fmt"
	"maps"
	"slices"
)

// Projection is the immutable product view of one committed journal prefix.
// Its rows and returned state are borrowed: readers must never mutate them.
// Advancing the prefix shares unchanged tables and index buckets. Store and
// Live hold the same projection; live observations remain separately owned.
type Projection struct {
	DurableState
	indexes   projectionIndexes
	addresses RowMap[string]
}

// NewProjection imports an owned snapshot. Normal journal replay uses advance,
// which copies only the tables and relationship buckets touched by a batch.
func NewProjection(state DurableState) *Projection {
	return newProjection(state.Clone())
}

func newProjection(state DurableState) *Projection {
	p := &Projection{DurableState: state}
	p.indexes = updateIndexes(projectionIndexes{}, DurableState{}, state, Diff(DurableState{}, state))
	for id, a := range state.Assignments.All() {
		for _, address := range []string{a.AllocationIPv4, a.AllocationIPv6} {
			if address != "" {
				p.addresses.Set(address, id)
			}
		}
	}
	return p
}

func (p *Projection) advance(entry Entry) (*Projection, Batch, error) {
	state, batch, err := p.DurableState.applyEntry(entry)
	if err != nil {
		return p, Batch{}, err
	}
	next, err := p.project(state, batch)
	if err != nil {
		return p, Batch{}, err
	}
	return next, batch, nil
}

// Preview applies the resolved SQL effects before their agent revision bumps.
// It never publishes attempted state or advances the committed prefix.
func (p *Projection) Preview(batch Batch) (*Projection, error) {
	state, err := p.DurableState.applyBatch(batch)
	if err != nil {
		return nil, err
	}
	return p.project(state, batch)
}

func (p *Projection) project(state DurableState, batch Batch) (*Projection, error) {
	addresses := p.addresses
	affected := make(map[string]struct{}, len(batch.Assignments))
	// Release all changed reservations before validating replacements, allowing
	// address swaps and delete/recreate within a single atomic batch.
	for _, c := range batch.Assignments {
		affected[c.Key] = struct{}{}
		a := p.Assignments.Get(c.Key)
		for _, address := range []string{a.AllocationIPv4, a.AllocationIPv6} {
			if address != "" {
				addresses.Delete(address)
			}
		}
	}
	for _, c := range batch.Agents {
		if c.Value == nil {
			for id := range p.indexes.assignmentsByAgent.Get(c.Key) {
				affected[id] = struct{}{}
			}
		}
	}
	for _, c := range batch.Services {
		if c.Value == nil {
			for id := range p.indexes.assignmentsByService.Get(c.Key) {
				affected[id] = struct{}{}
			}
		}
	}
	for _, c := range batch.Deployments {
		if c.Value == nil {
			for id := range p.indexes.assignmentsByDeployment.Get(c.Key) {
				affected[id] = struct{}{}
			}
		}
	}
	for id := range affected {
		a, exists := state.Assignments.Lookup(id)
		if !exists {
			continue
		}
		if err := state.validateAssignment(id, a); err != nil {
			return nil, err
		}
	}
	for _, c := range batch.Assignments {
		if c.Value == nil {
			continue
		}
		for _, address := range []string{c.Value.AllocationIPv4, c.Value.AllocationIPv6} {
			if address == "" {
				continue
			}
			if owner, exists := addresses.Lookup(address); exists {
				return nil, fmt.Errorf("address %s reserved by both %s and %s", address, owner, c.Key)
			}
			addresses.Set(address, c.Key)
		}
	}
	return &Projection{DurableState: state, indexes: updateIndexes(p.indexes, p.DurableState, state, batch), addresses: addresses}, nil
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
type relation struct{ RowMap[map[string]int] }

func (r relation) keys(key string) []string {
	keys := slices.Collect(maps.Keys(r.Get(key)))
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
		e.owned = make(map[string]bool)
	}
	bucket := e.next.Get(key)
	if !e.owned[key] {
		bucket = maps.Clone(bucket)
		e.owned[key] = true
	}
	if bucket == nil {
		bucket = make(map[string]int)
	}
	n := bucket[member] + delta
	if n > 0 {
		bucket[member] = n
	} else {
		delete(bucket, member)
	}
	if len(bucket) == 0 {
		e.next.Delete(key)
	} else {
		e.next.Set(key, bucket)
	}
}

type projectionIndexes struct {
	assignmentsByAgent, assignmentsByService    relation
	assignmentsByDeployment                     relation
	servicesByEnvironment, domainsByService     relation
	environmentsByAgent, agentsByEnvironment    relation
	environmentsByProject, volumesByEnvironment relation
	volumesByAgent, destructionsByAgent         relation
}

func updateIndexes(previous projectionIndexes, before, after DurableState, batch Batch) projectionIndexes {
	assignmentsByAgent, assignmentsByService := editRelation(previous.assignmentsByAgent), editRelation(previous.assignmentsByService)
	assignmentsByDeployment := editRelation(previous.assignmentsByDeployment)
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
			for id := range previous.assignmentsByService.Get(c.Key) {
				changedAssignments[id] = struct{}{}
			}
		}
	}
	for id := range changedAssignments {
		old, next := before.Assignments.Get(id), after.Assignments.Get(id)
		if old.DeploymentID != next.DeploymentID {
			assignmentsByDeployment.add(old.DeploymentID, id, -1)
			assignmentsByDeployment.add(next.DeploymentID, id, 1)
		}
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
	return projectionIndexes{assignmentsByAgent.next, assignmentsByService.next, assignmentsByDeployment.next, servicesByEnvironment.next, domainsByService.next,
		environmentsByAgent.next, agentsByEnvironment.next, environmentsByProject.next, volumesByEnvironment.next,
		volumesByAgent.next, destructionsByAgent.next}
}
