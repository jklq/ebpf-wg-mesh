package delivery

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
)

func (l *Live) requireAccepting() error {
	if l == nil || !l.serving || !l.accepting {
		return ErrNotLiveOwner
	}
	return nil
}

func (l *Live) BeginSession(agentID, sessionID string, inventory []string, assigned []string, ready bool) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	agentID = strings.TrimSpace(agentID)
	sessionID = strings.TrimSpace(sessionID)
	if agentID == "" || sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return err
	}
	now := l.now().UTC()
	if previous, ok := l.sessions[agentID]; ok && previous.SessionID != sessionID {
		l.invalidateSessionLocked(agentID, previous.SessionID)
	}
	session := &AgentSession{
		AgentID: agentID, SessionID: sessionID, LastContact: now,
		Ready: ready, Reachable: true,
	}
	session.Reconciled = inventoryReconciled(assigned, inventory)
	l.sessions[agentID] = session
	if session.Reconciled {
		l.admitted[agentID] = struct{}{}
	} else {
		delete(l.admitted, agentID)
	}
	l.resetTimerLocked(agentID)
	l.touchLiveLocked()
	return nil
}

// InitSessionVersions seeds accepted per-stream versions from hello.
func (l *Live) InitSessionVersions(agentID, sessionID string, accepted SyncVersions) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[strings.TrimSpace(agentID)]
	if !ok || session.SessionID != strings.TrimSpace(sessionID) {
		return
	}
	session.AcceptedCursor = accepted.Cursor
	session.AcceptedNodeConfig = accepted.NodeConfig
	session.AcceptedCredentials = accepted.Credentials
	session.AcceptedReplicas = accepted.Replicas
}

func inventoryReconciled(assigned, inventory []string) bool {
	if len(assigned) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(inventory))
	for _, id := range inventory {
		id = strings.TrimSpace(id)
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, id := range assigned {
		if _, ok := seen[strings.TrimSpace(id)]; !ok {
			return false
		}
	}
	return true
}

func (l *Live) invalidateSessionLocked(agentID, sessionID string) {
	for key, obs := range l.observations {
		if obs.AgentID == agentID && obs.SessionID == sessionID {
			delete(l.observations, key)
		}
	}
	if timer, ok := l.timers[agentID]; ok {
		timer.Stop()
		delete(l.timers, agentID)
	}
	delete(l.admitted, agentID)
}

func (l *Live) resetTimerLocked(agentID string) {
	if timer, ok := l.timers[agentID]; ok {
		timer.Stop()
	}
	ttl := l.ttl
	timer := time.AfterFunc(ttl, func() { l.expire(agentID) })
	l.timers[agentID] = timer
}

func (l *Live) expire(agentID string) {
	l.mu.Lock()
	session, ok := l.sessions[agentID]
	if !ok || !l.serving {
		l.mu.Unlock()
		return
	}
	session.Reachable = false
	session.Ready = false
	delete(l.admitted, agentID)
	l.enqueueEvalLocked(liveEval{Kind: liveEvalExpiry, AgentID: agentID})
	l.touchLiveLocked()
	l.mu.Unlock()
}

func (l *Live) Heartbeat(agentID, sessionID string, ready bool) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return err
	}
	session, ok := l.sessions[strings.TrimSpace(agentID)]
	if !ok || session.SessionID != strings.TrimSpace(sessionID) {
		return ErrStaleAgentSession
	}
	session.LastContact = l.now().UTC()
	session.Ready = ready
	session.Reachable = true
	if session.Reconciled {
		l.admitted[session.AgentID] = struct{}{}
	}
	l.resetTimerLocked(session.AgentID)
	return nil
}

func (l *Live) EndSession(agentID, sessionID string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[strings.TrimSpace(agentID)]
	if !ok || session.SessionID != strings.TrimSpace(sessionID) {
		return nil
	}
	session.Ready = false
	session.Reachable = false
	delete(l.admitted, session.AgentID)
	if timer, ok := l.timers[session.AgentID]; ok {
		timer.Stop()
		delete(l.timers, session.AgentID)
	}
	l.touchLiveLocked()
	return nil
}

func (l *Live) AcceptReport(agentID, sessionID string, sequence uint64, inventory []string, ready bool) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	if sequence == 0 || sequence > math.MaxInt64 {
		return fmt.Errorf("%w: observation_sequence must be between 1 and %d", ErrStaleObservation, int64(math.MaxInt64))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return err
	}
	session, ok := l.sessions[strings.TrimSpace(agentID)]
	if !ok || session.SessionID != strings.TrimSpace(sessionID) {
		return ErrStaleAgentSession
	}
	if sequence <= session.Sequence {
		return fmt.Errorf("%w: report sequence %d follows %d", ErrStaleObservation, sequence, session.Sequence)
	}
	session.Sequence = sequence
	session.LastContact = l.now().UTC()
	session.Ready = ready
	session.Reachable = true
	// The agent's latest status report is the authoritative inventory, so
	// admission is promoted as soon as the inventory catches up. It is never
	// demoted here: an admitted agent keeps receiving work.
	session.Reconciled = session.Reconciled || inventoryReconciled(l.assignedIDsLocked(session.AgentID), inventory)
	if session.Reconciled {
		l.admitted[session.AgentID] = struct{}{}
	}
	l.resetTimerLocked(session.AgentID)
	return nil
}

func observationUnchanged(previous AllocationObservation, next AllocationObservation) bool {
	return previous.AppliedSpecRevision == next.AppliedSpecRevision &&
		previous.AppliedGeneration == next.AppliedGeneration &&
		previous.Healthy == next.Healthy &&
		observationPhaseClass(previous.Phase) == observationPhaseClass(next.Phase) &&
		previous.Restart.GetCrashLoop() == next.Restart.GetCrashLoop() &&
		slices.Equal(previous.HealthyIPv4Ports, next.HealthyIPv4Ports) &&
		slices.Equal(previous.HealthyIPv6Ports, next.HealthyIPv6Ports)
}

func observationPhaseClass(phase string) int {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "drained":
		return 0
	case "crashloop":
		return 1
	case "error", "failed", "unhealthy", "stopped":
		return 2
	default:
		return 3
	}
}

func (l *Live) RecordObservation(obs AllocationObservation) (ObservationOutcome, error) {
	if l == nil {
		return ObservationOutcome{}, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return ObservationOutcome{}, err
	}
	session, ok := l.sessions[obs.AgentID]
	if !ok || session.SessionID != obs.SessionID {
		return ObservationOutcome{}, ErrStaleAgentSession
	}
	key := liveObsKey{AllocationID: obs.AllocationID, Generation: obs.RolloutGeneration}
	previous, exists := l.observations[key]
	if !exists {
		l.observations[key] = obs
		l.touchLiveLocked()
		return ObservationOutcome{Changed: true, StatusInvalidated: true}, nil
	}
	if observationUnchanged(previous, obs) {
		l.observations[key] = obs
		if observationStatusChanged(previous, obs) {
			return ObservationOutcome{StatusInvalidated: true}, nil
		}
		return ObservationOutcome{}, nil
	}
	l.observations[key] = obs
	l.touchLiveLocked()
	return ObservationOutcome{Changed: true, StatusInvalidated: true}, nil
}

// observationStatusChanged reports whether any rendered status field differs.
func observationStatusChanged(previous, next AllocationObservation) bool {
	return previous.AppliedSpecRevision != next.AppliedSpecRevision ||
		previous.AppliedGeneration != next.AppliedGeneration ||
		previous.Phase != next.Phase ||
		previous.Message != next.Message ||
		previous.Healthy != next.Healthy ||
		!slices.Equal(previous.HealthyIPv4Ports, next.HealthyIPv4Ports) ||
		!slices.Equal(previous.HealthyIPv6Ports, next.HealthyIPv6Ports) ||
		!restartEvidenceEqual(previous.Restart, next.Restart)
}

func restartEvidenceEqual(previous, next *platformv1.RestartObservation) bool {
	if previous == nil {
		previous = &platformv1.RestartObservation{}
	}
	if next == nil {
		next = &platformv1.RestartObservation{}
	}
	return proto.Equal(previous, next)
}

func (l *Live) Observation(allocationID string, generation int64) (AllocationObservation, bool) {
	if l == nil {
		return AllocationObservation{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	obs, ok := l.observations[liveObsKey{AllocationID: allocationID, Generation: generation}]
	return obs, ok
}

func (l *Live) Session(agentID string) (AgentSession, bool) {
	if l == nil {
		return AgentSession{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[agentID]
	if !ok || session == nil {
		return AgentSession{}, false
	}
	return *session, true
}

func (l *Live) Admitted(agentID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return false
	}
	_, ok := l.admitted[agentID]
	if !ok {
		return false
	}
	session, exists := l.sessions[agentID]
	return exists && session.Reachable && session.Ready && session.Reconciled
}

func appendOffered(history []string, version string) []string {
	if version == "" {
		return history
	}
	history = append(history, version)
	if len(history) > offerHistoryLimit {
		history = history[len(history)-offerHistoryLimit:]
	}
	return history
}

func (l *Live) Grant(agentID, sessionID string, epoch uint64, offered SyncVersions) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return err
	}
	session, ok := l.sessions[agentID]
	if !ok || session.SessionID != sessionID || !session.Reachable {
		return ErrStaleAgentSession
	}
	session.OfferedEpoch = int64(epoch)
	session.OfferedCursor = offered.Cursor
	session.OfferedNodeConfig = appendOffered(session.OfferedNodeConfig, offered.NodeConfig)
	session.OfferedCredentials = appendOffered(session.OfferedCredentials, offered.Credentials)
	session.OfferedReplicas = appendOffered(session.OfferedReplicas, offered.Replicas)
	return nil
}

func (l *Live) Acknowledge(agentID, sessionID string, epoch uint64, accepted SyncVersions) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[agentID]
	if !ok || session.SessionID != sessionID {
		return ErrStaleAgentSession
	}
	cursor := accepted.Cursor
	if cursor < 0 || session.OfferedEpoch != int64(epoch) || session.OfferedCursor < cursor {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	if session.AcceptedEpoch > int64(epoch) || (session.AcceptedEpoch == int64(epoch) && session.AcceptedCursor > cursor) {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	// Hash versions are unordered: an ack must match a version offered in
	// this session or repeat the accepted one (idempotent duplicate).
	if accepted.NodeConfig != "" && accepted.NodeConfig != session.AcceptedNodeConfig && !slices.Contains(session.OfferedNodeConfig, accepted.NodeConfig) {
		return fmt.Errorf("stale node-config acknowledgement")
	}
	if accepted.Credentials != "" && accepted.Credentials != session.AcceptedCredentials && !slices.Contains(session.OfferedCredentials, accepted.Credentials) {
		return fmt.Errorf("stale credentials acknowledgement")
	}
	if accepted.Replicas != "" && accepted.Replicas != session.AcceptedReplicas && !slices.Contains(session.OfferedReplicas, accepted.Replicas) {
		return fmt.Errorf("stale replicas acknowledgement")
	}
	session.AcceptedEpoch = int64(epoch)
	session.AcceptedCursor = cursor
	if accepted.NodeConfig != "" {
		session.AcceptedNodeConfig = accepted.NodeConfig
	}
	if accepted.Credentials != "" {
		session.AcceptedCredentials = accepted.Credentials
	}
	if accepted.Replicas != "" {
		session.AcceptedReplicas = accepted.Replicas
	}
	return nil
}

func (l *Live) ExpireForTest(agentID string) {
	if l == nil {
		return
	}
	l.expire(agentID)
}

func (l *Live) SetLastContactForTest(agentID string, at time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if session, ok := l.sessions[agentID]; ok {
		session.LastContact = at.UTC()
		if l.now().UTC().Sub(session.LastContact) >= l.ttl {
			session.Reachable = false
			session.Ready = false
			delete(l.admitted, agentID)
		}
	}
}
