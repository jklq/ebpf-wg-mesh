package delivery

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"

	"google.golang.org/protobuf/encoding/protojson"
)

func newLiveIndexes() liveIndexes {
	return liveIndexes{
		environmentsByAgent:   make(map[string][]string),
		agentsByEnvironment:   make(map[string][]string),
		assignmentsByAgent:    make(map[string][]string),
		assignmentsByService:  make(map[string][]string),
		domainsByService:      make(map[string][]string),
		servicesByEnvironment: make(map[string][]string),
	}
}

func (l *Live) ApplyDurable(state journal.DurableState) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyDurableLocked(state)
}

func (l *Live) applyDurableLocked(state journal.DurableState) {
	if state.ClusterID != "" && l.durableIndexes[state.ClusterID] >= state.LogIndex && l.durableIndexes[state.ClusterID] != 0 {
		return
	}
	previous := l.durable
	l.durable = state.Clone()
	if state.ClusterID != "" && state.LogIndex > l.durableIndexes[state.ClusterID] {
		l.durableIndexes[state.ClusterID] = state.LogIndex
	}
	l.rebuildIndexesLocked()
	l.liveIndex++
	for id, agent := range l.durable.Agents {
		if before, ok := previous.Agents[id]; ok && before.DesiredRevision == agent.DesiredRevision {
			continue
		}
		l.notifyLocked(id)
	}
}

func (l *Live) rebuildIndexesLocked() {
	idx := newLiveIndexes()
	for id, assignment := range l.durable.Assignments {
		idx.assignmentsByAgent[assignment.AgentID] = append(idx.assignmentsByAgent[assignment.AgentID], id)
		idx.assignmentsByService[assignment.ServiceID] = append(idx.assignmentsByService[assignment.ServiceID], id)
		if service, ok := l.durable.Services[assignment.ServiceID]; ok && assignment.RolloutState != AllocationRolloutLost {
			idx.environmentsByAgent[assignment.AgentID] = append(idx.environmentsByAgent[assignment.AgentID], service.EnvironmentID)
			idx.agentsByEnvironment[service.EnvironmentID] = append(idx.agentsByEnvironment[service.EnvironmentID], assignment.AgentID)
		}
	}
	for hostname, domain := range l.durable.Domains {
		idx.domainsByService[domain.ServiceID] = append(idx.domainsByService[domain.ServiceID], hostname)
	}
	for id, service := range l.durable.Services {
		idx.servicesByEnvironment[service.EnvironmentID] = append(idx.servicesByEnvironment[service.EnvironmentID], id)
	}
	for agentID := range idx.assignmentsByAgent {
		slices.Sort(idx.assignmentsByAgent[agentID])
	}
	for serviceID := range idx.assignmentsByService {
		slices.Sort(idx.assignmentsByService[serviceID])
	}
	for serviceID := range idx.domainsByService {
		slices.Sort(idx.domainsByService[serviceID])
	}
	for environmentID := range idx.servicesByEnvironment {
		slices.Sort(idx.servicesByEnvironment[environmentID])
	}
	for agentID, ids := range idx.environmentsByAgent {
		slices.Sort(ids)
		idx.environmentsByAgent[agentID] = slices.Compact(ids)
	}
	for environmentID, ids := range idx.agentsByEnvironment {
		slices.Sort(ids)
		idx.agentsByEnvironment[environmentID] = slices.Compact(ids)
	}
	l.indexes = idx
}

func (l *Live) touchLiveLocked() {
	l.liveIndex++
	l.notifyLocked("")
}

func (l *Live) Position() LivePosition {
	if l == nil {
		return LivePosition{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.positionLocked()
}

func (l *Live) positionLocked() LivePosition {
	pos := LivePosition{
		AcceptedDurable: l.durable.LogIndex,
		AppliedLive:     l.liveIndex,
		Ready:           l.serving && l.accepting,
	}
	for _, session := range l.sessions {
		if session == nil || session.LastContact.IsZero() {
			continue
		}
		if pos.ObservationFreshness.IsZero() || session.LastContact.After(pos.ObservationFreshness) {
			pos.ObservationFreshness = session.LastContact
		}
	}
	for _, obs := range l.observations {
		if obs.ObservedAt.IsZero() {
			continue
		}
		if pos.ObservationFreshness.IsZero() || obs.ObservedAt.After(pos.ObservationFreshness) {
			pos.ObservationFreshness = obs.ObservedAt
		}
	}
	return pos
}

func (l *Live) AuthorityEpoch() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authorityEpoch
}

func (l *Live) DesiredRevision(agentID string) (int64, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	agent, ok := l.durable.Agents[agentID]
	if !ok {
		return 0, false
	}
	return agent.DesiredRevision, true
}

func (l *Live) Watch(agentID string) (<-chan struct{}, func()) {
	if l == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.watchLocked(strings.TrimSpace(agentID))
}

func (l *Live) watchLocked(agentID string) (<-chan struct{}, func()) {
	w := &liveWatcher{agentID: agentID, ch: make(chan struct{}, 1)}
	l.watchers = append(l.watchers, w)
	return w.ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, existing := range l.watchers {
			if existing != w {
				continue
			}
			l.watchers[i] = l.watchers[len(l.watchers)-1]
			l.watchers[len(l.watchers)-1] = nil
			l.watchers = l.watchers[:len(l.watchers)-1]
			close(w.ch)
			return
		}
	}
}

func (l *Live) Notify(agentID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notifyLocked(strings.TrimSpace(agentID))
}

func (l *Live) notifyLocked(agentID string) {
	for _, w := range l.watchers {
		if agentID != "" && w.agentID != "" && w.agentID != agentID {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

func (l *Live) Durable() journal.DurableState {
	if l == nil {
		return journal.DurableState{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.durable.Clone()
}

func (l *Live) AgentIDs() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.agentIDsLocked()
}

// AgentIDsIfServing performs the live-role check and the read under a single
// lock so a concurrent resign cannot return an empty result with a nil error.
func (l *Live) AgentIDsIfServing() ([]string, error) {
	if l == nil {
		return nil, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return nil, ErrNotLiveOwner
	}
	return l.agentIDsLocked(), nil
}

func (l *Live) agentIDsLocked() []string {
	ids := make([]string, 0, len(l.durable.Agents))
	for id := range l.durable.Agents {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if n := l.durable.Agents[a].CreatedAt.Compare(l.durable.Agents[b].CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	return ids
}

func (l *Live) Agents() []AgentRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.agentsLocked()
}

// AgentsIfServing performs the live-role check and the read under a single
// lock so a concurrent resign cannot return an empty result with a nil error.
func (l *Live) AgentsIfServing() ([]AgentRecord, error) {
	if l == nil {
		return nil, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return nil, ErrNotLiveOwner
	}
	return l.agentsLocked(), nil
}

func (l *Live) agentsLocked() []AgentRecord {
	out := make([]AgentRecord, 0, len(l.durable.Agents))
	ids := l.agentIDsLocked()
	for _, id := range ids {
		out = append(out, l.overlayAgentLocked(agentRecordFromDurable(l.durable.Agents[id], l.durable.Administration[id])))
	}
	return out
}

func (l *Live) Agent(agentID string) (AgentRecord, bool) {
	if l == nil {
		return AgentRecord{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.agentLocked(agentID)
}

// AgentIfServing performs the live-role check and the read under a single lock
// so a concurrent resign cannot report a missing agent with a nil error.
func (l *Live) AgentIfServing(agentID string) (AgentRecord, error) {
	if l == nil {
		return AgentRecord{}, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return AgentRecord{}, ErrNotLiveOwner
	}
	rec, ok := l.agentLocked(agentID)
	if !ok {
		return AgentRecord{}, sql.ErrNoRows
	}
	return rec, nil
}

func (l *Live) agentLocked(agentID string) (AgentRecord, bool) {
	reg, ok := l.durable.Agents[agentID]
	if !ok {
		return AgentRecord{}, false
	}
	return l.overlayAgentLocked(agentRecordFromDurable(reg, l.durable.Administration[agentID])), true
}

func (l *Live) overlayAgentLocked(rec AgentRecord) AgentRecord {
	session, ok := l.sessions[rec.ID]
	if !ok {
		return overlayAgentAbsent(rec)
	}
	rec.LastSeenAt = session.LastContact
	if rec.LifecycleState == AgentStateEnrolling || rec.LifecycleState == AgentStateRetired {
		return rec
	}
	expired := l.serving && l.now().UTC().Sub(session.LastContact) >= l.ttl
	if !session.Ready || !session.Reachable || expired {
		rec.StateBeforeUnavailable = rec.LifecycleState
		rec.LifecycleState = AgentStateUnavailable
	}
	return rec
}

func (l *Live) AllocationsByService(serviceID string) []AllocationRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allocationsByServiceLocked(serviceID)
}

// AllocationsByServiceIfServing performs the live-role check and the read under
// a single lock so a concurrent resign cannot return an empty slice with a nil
// error.
func (l *Live) AllocationsByServiceIfServing(serviceID string) ([]AllocationRecord, error) {
	if l == nil {
		return nil, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return nil, ErrNotLiveOwner
	}
	return l.allocationsByServiceLocked(serviceID), nil
}

// AllocationsByEnvironmentIfServing returns one consistent allocation view for
// every service in an environment. Including services with no allocations lets
// callers distinguish an empty live result from a missing lookup.
func (l *Live) AllocationsByEnvironmentIfServing(environmentID string) (map[string][]AllocationRecord, error) {
	if l == nil {
		return nil, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return nil, ErrNotLiveOwner
	}
	serviceIDs := l.indexes.servicesByEnvironment[environmentID]
	out := make(map[string][]AllocationRecord, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		out[serviceID] = l.allocationsByServiceLocked(serviceID)
	}
	return out, nil
}

func (l *Live) allocationsByServiceLocked(serviceID string) []AllocationRecord {
	ids := l.indexes.assignmentsByService[serviceID]
	out := make([]AllocationRecord, 0, len(ids))
	for _, id := range ids {
		assignment, ok := l.durable.Assignments[id]
		if !ok {
			continue
		}
		out = append(out, l.overlayAllocationLocked(allocationRecordFromAssignment(l.durable, assignment)))
	}
	return out
}

func (l *Live) AllocationsByAgent(agentID string) []AllocationRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := l.indexes.assignmentsByAgent[agentID]
	out := make([]AllocationRecord, 0, len(ids))
	for _, id := range ids {
		assignment, ok := l.durable.Assignments[id]
		if !ok {
			continue
		}
		out = append(out, l.overlayAllocationLocked(allocationRecordFromAssignment(l.durable, assignment)))
	}
	return out
}

func (l *Live) overlayAllocationLocked(rec AllocationRecord) AllocationRecord {
	var session AgentSession
	var hasSession bool
	if s, ok := l.sessions[rec.AgentID]; ok && s != nil {
		session, hasSession = *s, true
	}
	obs, hasObs := l.observations[liveObsKey{AllocationID: rec.ID, Generation: rec.DesiredRolloutGeneration}]
	return overlayAllocation(rec, session, hasSession, obs, hasObs, l.now().UTC(), l.ttl)
}

func (l *Live) AssignedIDs(agentID string) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.assignedIDsLocked(agentID)
}

func (l *Live) assignedIDsLocked(agentID string) []string {
	var ids []string
	for _, id := range l.indexes.assignmentsByAgent[agentID] {
		assignment := l.durable.Assignments[id]
		if assignment.RolloutState != AllocationRolloutLost {
			ids = append(ids, id)
		}
	}
	return ids
}

func (l *Live) InProgressRolloutServiceIDs() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[string]struct{})
	for _, rollout := range l.durable.Rollouts {
		service, ok := l.durable.Services[rollout.ServiceID]
		if !ok || service.CurrentRolloutGeneration != rollout.RolloutGeneration {
			continue
		}
		if rollout.State == rolloutStateInProgress {
			seen[rollout.ServiceID] = struct{}{}
		}
	}
	for _, assignment := range l.durable.Assignments {
		if assignment.RolloutState == AllocationRolloutDraining || assignment.RolloutState == AllocationRolloutWithdrawing {
			seen[assignment.ServiceID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (l *Live) PlacementCandidates() []placementCandidate {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.placementCandidatesLocked()
}

func (l *Live) placementCandidatesLocked() []placementCandidate {
	usage := make(map[string]struct{ count, cpu, memory int64 })
	for _, assignment := range l.durable.Assignments {
		if assignment.RolloutState == AllocationRolloutLost {
			continue
		}
		service := l.durable.Services[assignment.ServiceID]
		revision := l.durable.Revisions[fmt.Sprintf("%s/%d", assignment.ServiceID, service.CurrentSpecRevision)]
		spec, err := LoadServiceSpec(revision.SpecJSON)
		if err != nil {
			continue
		}
		runtime := serviceRuntime(spec)
		item := usage[assignment.AgentID]
		item.count++
		item.cpu += int64(runtime.GetCpuMillis())
		item.memory += int64(runtime.GetMemoryMebibytes())
		usage[assignment.AgentID] = item
	}
	var candidates []placementCandidate
	for id, agent := range l.durable.Agents {
		admin := l.durable.Administration[id]
		if admin.LifecycleState != string(AgentStateActive) {
			continue
		}
		if !l.admittedLocked(id) {
			continue
		}
		used := usage[id]
		candidates = append(candidates, placementCandidate{
			ID:                     id,
			Region:                 agent.Region,
			Zone:                   agent.Zone,
			FailureDomain:          agent.FailureDomain,
			RuntimeCapabilities:    decodeRuntimeCapabilities(agent.RuntimeCapabilities),
			CPUMillisCapacity:      max(agent.CPUMillisCapacity-agent.ReservedCPUMillis, 0),
			MemoryMebibytesCapcity: max(agent.MemoryMebibytesCapacity-agent.ReservedMemoryMebibytes, 0),
			ServiceCount:           used.count,
			UsedCPUMillis:          used.cpu,
			UsedMemoryMebibytes:    used.memory,
		})
	}
	slices.SortFunc(candidates, func(a, b placementCandidate) int {
		if a.ServiceCount != b.ServiceCount {
			if a.ServiceCount < b.ServiceCount {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return candidates
}

func (l *Live) admittedLocked(agentID string) bool {
	if !l.serving {
		return false
	}
	if _, ok := l.admitted[agentID]; !ok {
		return false
	}
	session, exists := l.sessions[agentID]
	return exists && session.Reachable && session.Ready && session.Reconciled
}

func (l *Live) AgentUsage() map[string]AgentUsage {
	if l == nil {
		return map[string]AgentUsage{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]AgentUsage, len(l.durable.Agents))
	for _, assignment := range l.durable.Assignments {
		if assignment.RolloutState == AllocationRolloutLost {
			continue
		}
		service := l.durable.Services[assignment.ServiceID]
		revision := l.durable.Revisions[fmt.Sprintf("%s/%d", assignment.ServiceID, service.CurrentSpecRevision)]
		spec, err := LoadServiceSpec(revision.SpecJSON)
		if err != nil {
			continue
		}
		runtime := serviceRuntime(spec)
		item := out[assignment.AgentID]
		item.Allocations++
		item.CPUMillis += int64(runtime.GetCpuMillis())
		item.MemoryMebibytes += int64(runtime.GetMemoryMebibytes())
		out[assignment.AgentID] = item
	}
	return out
}

type AgentUsage struct {
	Allocations     int32
	CPUMillis       int64
	MemoryMebibytes int64
}

func agentRecordFromDurable(reg journal.AgentRegistration, admin journal.AgentAdministration) AgentRecord {
	rec := AgentRecord{
		ID:                      reg.ID,
		Name:                    reg.Name,
		LifecycleState:          AgentLifecycleState(admin.LifecycleState),
		Region:                  reg.Region,
		Zone:                    reg.Zone,
		FailureDomain:           reg.FailureDomain,
		ReservedCPUMillis:       reg.ReservedCPUMillis,
		ReservedMemoryMebibytes: reg.ReservedMemoryMebibytes,
		AdvertiseAddr:           reg.AdvertiseAddr,
		WorkloadIPv4Subnet:      reg.WorkloadIPv4Subnet,
		WorkloadIPv6Subnet:      reg.WorkloadIPv6Subnet,
		WireGuardPublicKey:      reg.WireguardPublicKey,
		WireGuardListenPort:     int(reg.WireguardListenPort),
		WireGuardEndpoint:       reg.WireguardEndpoint,
		WireGuardIPv6:           reg.WireguardIPv6,
		CPUMillisCapacity:       reg.CPUMillisCapacity,
		MemoryMebibytesCapcity:  reg.MemoryMebibytesCapacity,
		RuntimeCapabilities:     decodeRuntimeCapabilities(reg.RuntimeCapabilities),
		SoftwareVersion:         reg.SoftwareVersion,
		MaintenanceMessage:      admin.MaintenanceMessage,
		LastSeenAt:              time.Unix(0, 0).UTC(),
	}
	if admin.CredentialRevokedAt != nil {
		rec.CredentialRevokedAt = sql.NullTime{Time: *admin.CredentialRevokedAt, Valid: true}
	}
	return rec
}

func allocationRecordFromAssignment(durable journal.DurableState, assignment journal.Assignment) AllocationRecord {
	service := durable.Services[assignment.ServiceID]
	environment := durable.Environments[service.EnvironmentID]
	rec := AllocationRecord{
		ID:                       assignment.ID,
		ServiceID:                assignment.ServiceID,
		ProjectID:                environment.ProjectID,
		EnvironmentID:            service.EnvironmentID,
		AgentID:                  assignment.AgentID,
		DesiredSpecRevision:      assignment.DesiredSpecRevision,
		DesiredRolloutGeneration: assignment.DesiredRolloutGeneration,
		Message:                  assignment.IntentMessage,
		AllocationIPv4:           assignment.AllocationIPv4,
		AllocationIPv6:           assignment.AllocationIPv6,
		OperatorRestartNonce:     assignment.OperatorRestartNonce,
		RolloutState:             assignment.RolloutState,
		CreatedAt:                assignment.CreatedAt,
		UpdatedAt:                assignment.UpdatedAt,
	}
	if assignment.DrainStartedAt != nil {
		rec.DrainStartedAt = sql.NullTime{Time: *assignment.DrainStartedAt, Valid: true}
	}
	if assignment.DrainDeadline != nil {
		rec.DrainDeadline = sql.NullTime{Time: *assignment.DrainDeadline, Valid: true}
	}
	return rec
}

func decodeRuntimeCapabilities(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

func domainTargetPortsFromDurable(durable journal.DurableState, hostnames []string, serviceID string) []int32 {
	var ports []int32
	for _, hostname := range hostnames {
		domain, ok := durable.Domains[hostname]
		if !ok || domain.ServiceID != serviceID {
			continue
		}
		ports = append(ports, int32(domain.TargetPort))
	}
	return ports
}

func (l *Live) rolloutSnapshot(serviceID string) (rolloutSnapshot, string, bool) {
	if l == nil {
		return rolloutSnapshot{}, "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return rolloutSnapshot{}, "", false
	}
	service, ok := l.durable.Services[serviceID]
	if !ok {
		return rolloutSnapshot{}, "", false
	}
	var rec rolloutRecord
	if rollout, ok := l.durable.Rollouts[fmt.Sprintf("%s/%d", serviceID, service.CurrentRolloutGeneration)]; ok {
		rec = rolloutRecord{
			ServiceID:           rollout.ServiceID,
			Generation:          rollout.RolloutGeneration,
			SpecRevision:        rollout.SpecRevision,
			State:               rollout.State,
			DesiredReplicaCount: int32(rollout.DesiredReplicaCount),
			ImageDigest:         rollout.ImageDigest,
			FailureReason:       rollout.FailureReason,
			TargetAllocationID:  rollout.TargetAllocationID,
			CreatedAt:           rollout.CreatedAt,
			ProgressAt:          rollout.ProgressAt,
			Strategy:            &platformv1.RollingStrategy{},
		}
		raw := strings.TrimSpace(string(rollout.StrategyJSON))
		if raw != "" && raw != "{}" && raw != "null" {
			if err := protojson.Unmarshal(rollout.StrategyJSON, rec.Strategy); err != nil {
				return rolloutSnapshot{}, "", false
			}
		}
		rec.Strategy = canonicalRollingStrategy(rec.Strategy)
	}
	removing := false
	for _, dep := range l.durable.Deployments {
		if dep.ServiceID == serviceID && dep.IsCurrent && dep.State == DeploymentStateDraining && dep.ReasonCode == reasonUserRemove {
			removing = true
			break
		}
	}
	return rolloutSnapshot{
		Rollout:     rec,
		Allocations: l.allocationsByServiceLocked(serviceID),
		Removing:    removing,
	}, service.EnvironmentID, true
}

func rolloutPlanNeedsTx(plan rolloutPlan) bool {
	return len(plan.Remove) > 0 || len(plan.Promote) > 0 || len(plan.Withdraw) > 0 ||
		plan.Failure != "" || plan.Complete || plan.CompleteRemoval || plan.PlacementSlots > 0
}
