package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/restartpolicy"
)

const LiveOwnerRedirectPrefix = "not the live owner; reconnect at "

type liveObsKey struct {
	AllocationID string
	Generation   int64
}

type AgentSession struct {
	AgentID        string
	SessionID      string
	Sequence       uint64
	LastContact    time.Time
	Ready          bool
	Reachable      bool
	OfferedEpoch   int64
	OfferedCursor  int64
	AcceptedEpoch  int64
	AcceptedCursor int64
	Reconciled     bool
}

type liveEval struct {
	Kind         string
	AllocationID string
	AgentID      string
}

type LivePosition struct {
	AcceptedDurable      int64
	AppliedLive          uint64
	ObservationFreshness time.Time
	Ready                bool
}

type liveWatcher struct {
	agentID string
	ch      chan struct{}
}

type liveIndexes struct {
	environmentsByAgent   map[string][]string
	agentsByEnvironment   map[string][]string
	assignmentsByAgent    map[string][]string
	assignmentsByService  map[string][]string
	domainsByService      map[string][]string
	servicesByEnvironment map[string][]string
}

const (
	liveEvalObservation = "observation"
	liveEvalExpiry      = "expiry"
	liveEvalDeadline    = "deadline"
	liveEvalRollout     = "rollout"
)

type Live struct {
	mu sync.Mutex

	now func() time.Time
	ttl time.Duration

	serving    bool
	publishing bool
	accepting  bool

	sessions     map[string]*AgentSession
	observations map[liveObsKey]AllocationObservation
	admitted     map[string]struct{}
	timers       map[string]*time.Timer
	deadlines    map[string]time.Time

	durable        journal.DurableState
	durableIndexes map[string]int64
	liveIndex      uint64
	authorityEpoch uint64
	indexes        liveIndexes
	watchers       []*liveWatcher

	evals chan liveEval
}

func NewLive() *Live {
	return &Live{
		now:            func() time.Time { return time.Now().UTC() },
		ttl:            AgentHealthyTTL,
		sessions:       make(map[string]*AgentSession),
		observations:   make(map[liveObsKey]AllocationObservation),
		admitted:       make(map[string]struct{}),
		timers:         make(map[string]*time.Timer),
		deadlines:      make(map[string]time.Time),
		durableIndexes: make(map[string]int64),
		indexes:        newLiveIndexes(),
		evals:          make(chan liveEval, 128),
	}
}

func (l *Live) Serving() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.serving
}

func (l *Live) Publishing() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.publishing
}

func (l *Live) SetPublishing(ok bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.publishing = ok && l.serving
	l.mu.Unlock()
}

func (l *Live) SetClock(now func() time.Time) {
	if l == nil || now == nil {
		return
	}
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}

func (l *Live) currentTime() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now == nil {
		return time.Now().UTC()
	}
	return l.now().UTC()
}

func (d *Delivery) BecomeLive(ctx context.Context) error {
	if d == nil || d.live == nil {
		return nil
	}
	return d.live.become(ctx, d.store.readState, d.store.readAuthorityEpoch)
}

func (d *Delivery) ResignLive() {
	if d != nil && d.live != nil {
		d.live.resign()
	}
}

func (d *Delivery) EvaluateObservedDeploymentForTest(ctx context.Context, allocationID string) error {
	return d.evaluateObservedDeployment(ctx, allocationID)
}

func (d *Delivery) ServeLive(ctx context.Context) error {
	if d == nil || d.live == nil {
		return nil
	}
	owned := false
	if !d.live.Serving() {
		if err := d.live.become(ctx, d.store.readState, d.store.readAuthorityEpoch); err != nil {
			return err
		}
		owned = true
	}
	if owned {
		defer d.live.resign()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case eval := <-d.live.evals:
			d.handleLiveEval(ctx, eval)
		case <-ticker.C:
			d.live.requestDueDeadlines()
		}
	}
}

func (l *Live) become(ctx context.Context, readState func(context.Context, func(*sql.Tx, journal.DurableState) error) error, readEpoch func(context.Context) (uint64, error)) error {
	// Reset only live-only state up front. The last-known-good durable snapshot
	// (applied at boot, before the singleton lease is acquired) stays readable
	// while the replacement loads, so concurrent readers never observe an empty
	// world mid-acquire. The guarded apply below still picks up the freshest
	// snapshot, including advances applied via the journal hook during the read.
	l.mu.Lock()
	l.resetEphemeralLocked()
	l.mu.Unlock()

	var durable journal.DurableState
	if err := readState(ctx, func(_ *sql.Tx, state journal.DurableState) error {
		durable = state
		return nil
	}); err != nil {
		return err
	}
	var epoch uint64
	if readEpoch != nil {
		var err error
		epoch, err = readEpoch(ctx)
		if err != nil {
			return err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyDurableLocked(durable)
	durable = l.durable.Clone()
	l.serving = true
	l.authorityEpoch = epoch
	for id, assignment := range durable.Assignments {
		if assignment.DrainDeadline != nil && !assignment.DrainDeadline.IsZero() {
			l.deadlines[id] = assignment.DrainDeadline.UTC()
		}
	}
	for _, rollout := range durable.Rollouts {
		if rollout.State == rolloutStateInProgress {
			l.enqueueEvalLocked(liveEval{Kind: liveEvalRollout})
			break
		}
	}
	l.publishing = true
	l.accepting = true
	return nil
}

func (l *Live) resetLocked() {
	l.resetEphemeralLocked()
	l.authorityEpoch = 0
	l.durable = journal.DurableState{}
	l.durableIndexes = make(map[string]int64)
	l.indexes = newLiveIndexes()
}

func (l *Live) resetEphemeralLocked() {
	for _, timer := range l.timers {
		timer.Stop()
	}
	l.serving = false
	l.publishing = false
	l.accepting = false
	l.sessions = make(map[string]*AgentSession)
	l.observations = make(map[liveObsKey]AllocationObservation)
	l.admitted = make(map[string]struct{})
	l.timers = make(map[string]*time.Timer)
	l.deadlines = make(map[string]time.Time)
	for {
		select {
		case <-l.evals:
		default:
			l.touchLiveLocked()
			return
		}
	}
}

func (l *Live) resign() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetLocked()
}

func (l *Live) enqueueEvalLocked(eval liveEval) {
	select {
	case l.evals <- eval:
	default:
	}
}

func (l *Live) requestDueDeadlines() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return
	}
	now := l.now().UTC()
	due := false
	for id, at := range l.deadlines {
		if !at.After(now) {
			delete(l.deadlines, id)
			due = true
		}
	}
	if due {
		l.enqueueEvalLocked(liveEval{Kind: liveEvalDeadline})
	}
}

func (l *Live) ScheduleDeadline(allocationID string, at time.Time) {
	if l == nil || allocationID == "" || at.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.serving {
		return
	}
	l.deadlines[allocationID] = at.UTC()
}

func (d *Delivery) handleLiveEval(ctx context.Context, eval liveEval) {
	switch eval.Kind {
	case liveEvalObservation:
		if eval.AllocationID == "" {
			return
		}
		if err := d.evaluateObservedDeployment(ctx, eval.AllocationID); err != nil {
			return
		}
		if d.ingress != nil && d.live.Publishing() {
			d.ingress.RequestSync()
		}
	case liveEvalExpiry:
		if _, _, err := d.failoverServicesFromAgent(ctx, eval.AgentID, d.live.currentTime().Add(-AgentHealthyTTL)); err != nil {
			return
		}
		if d.ingress != nil && d.live.Publishing() {
			d.ingress.RequestSync()
		}
	case liveEvalDeadline, liveEvalRollout:
		_ = d.ReconcileRollouts(ctx)
	}
}

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
	// A session can begin before the agent has started every assigned
	// allocation (for example immediately after a partition heals). The agent's
	// latest status report is the authoritative inventory, so admission is
	// promoted as soon as the inventory catches up instead of being frozen
	// unreconciled until the next reconnect. It is never demoted here: an
	// already-admitted agent keeps receiving work, exactly as before this
	// report-driven check existed.
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

func (l *Live) RecordObservation(obs AllocationObservation) (changed bool, err error) {
	if l == nil {
		return false, ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return false, err
	}
	session, ok := l.sessions[obs.AgentID]
	if !ok || session.SessionID != obs.SessionID {
		return false, ErrStaleAgentSession
	}
	key := liveObsKey{AllocationID: obs.AllocationID, Generation: obs.RolloutGeneration}
	previous, exists := l.observations[key]
	if exists && observationUnchanged(previous, obs) {
		return false, nil
	}
	l.observations[key] = obs
	l.touchLiveLocked()
	return true, nil
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

func (l *Live) Grant(agentID, sessionID string, epoch uint64, cursor int64) error {
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
	session.OfferedCursor = cursor
	return nil
}

func (l *Live) Acknowledge(agentID, sessionID string, epoch uint64, cursor int64) error {
	if l == nil {
		return ErrNotLiveOwner
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[agentID]
	if !ok || session.SessionID != sessionID {
		return ErrStaleAgentSession
	}
	if cursor < 0 || session.OfferedEpoch != int64(epoch) || session.OfferedCursor < cursor {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	if session.AcceptedEpoch > int64(epoch) || (session.AcceptedEpoch == int64(epoch) && session.AcceptedCursor > cursor) {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	session.AcceptedEpoch = int64(epoch)
	session.AcceptedCursor = cursor
	return nil
}

func (l *Live) OverlayAgent(rec AgentRecord) AgentRecord {
	if l == nil {
		return overlayAgentAbsent(rec)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
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

func overlayAgentAbsent(rec AgentRecord) AgentRecord {
	if rec.LifecycleState != AgentStateEnrolling && rec.LifecycleState != AgentStateRetired {
		rec.StateBeforeUnavailable = rec.LifecycleState
		rec.LifecycleState = AgentStateUnavailable
	}
	rec.LastSeenAt = time.Unix(0, 0).UTC()
	return rec
}

func (l *Live) OverlayAllocation(rec AllocationRecord) AllocationRecord {
	var session AgentSession
	var hasSession bool
	var obs AllocationObservation
	var hasObs bool
	var now time.Time
	ttl := AgentHealthyTTL
	if l != nil {
		l.mu.Lock()
		if s, ok := l.sessions[rec.AgentID]; ok {
			session, hasSession = *s, true
		}
		obs, hasObs = l.observations[liveObsKey{AllocationID: rec.ID, Generation: rec.DesiredRolloutGeneration}]
		now = l.now().UTC()
		ttl = l.ttl
		l.mu.Unlock()
	}
	return overlayAllocation(rec, session, hasSession, obs, hasObs, now, ttl)
}

func overlayAllocation(rec AllocationRecord, session AgentSession, hasSession bool, obs AllocationObservation, hasObs bool, now time.Time, ttl time.Duration) AllocationRecord {
	expired := !hasSession || !session.Reachable || (!now.IsZero() && now.Sub(session.LastContact) >= ttl)
	if rec.RolloutState == AllocationRolloutLost || expired {
		rec.Phase = "Unavailable"
		rec.Healthy = false
		rec.HealthyIPv4Ports = nil
		rec.HealthyIPv6Ports = nil
		if rec.Message == "" && hasObs {
			rec.Message = obs.Message
		}
		return rec
	}
	if rec.RolloutState == AllocationRolloutWithdrawing {
		rec.Phase = "Withdrawing"
		rec.Healthy = false
		rec.HealthyIPv4Ports = nil
		rec.HealthyIPv6Ports = nil
		return rec
	}
	if rec.RolloutState == AllocationRolloutDraining && (!hasObs || obs.Phase != "Drained") {
		rec.Phase = "Draining"
		rec.Healthy = false
		rec.HealthyIPv4Ports = nil
		rec.HealthyIPv6Ports = nil
		if rec.Message == "" && hasObs {
			rec.Message = obs.Message
		}
		return rec
	}
	if !hasObs {
		if rec.Phase == "" {
			rec.Phase = "Pending"
		}
		rec.Healthy = false
		rec.AppliedSpecRevision = 0
		rec.AppliedRolloutGeneration = 0
		return rec
	}
	rec.AppliedSpecRevision = obs.AppliedSpecRevision
	rec.AppliedRolloutGeneration = obs.AppliedGeneration
	rec.Phase = obs.Phase
	if rec.Message == "" {
		rec.Message = obs.Message
	}
	rec.Healthy = obs.Healthy
	rec.HealthyIPv4Ports = append([]int32(nil), obs.HealthyIPv4Ports...)
	rec.HealthyIPv6Ports = append([]int32(nil), obs.HealthyIPv6Ports...)
	rec.Restart = obs.Restart
	if obs.ObservedAt.After(rec.UpdatedAt) {
		rec.UpdatedAt = obs.ObservedAt
	}
	if rec.Phase == restartpolicy.PhaseCrashLoop {
		rec.Healthy = false
	}
	return rec
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

func LiveOwnerRedirectMessage(addr string) string {
	return LiveOwnerRedirectPrefix + strings.TrimSpace(addr)
}

func ParseLiveOwnerRedirect(message string) (string, bool) {
	if !strings.HasPrefix(message, LiveOwnerRedirectPrefix) {
		return "", false
	}
	addr := strings.TrimSpace(strings.TrimPrefix(message, LiveOwnerRedirectPrefix))
	return addr, addr != ""
}
