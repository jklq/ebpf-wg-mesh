package delivery

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
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
	// Offered versions accumulate per session: a lagging cumulative ack must
	// still validate against the batch it acknowledges.
	OfferedNodeConfig   []string
	AcceptedNodeConfig  string
	OfferedCredentials  []string
	AcceptedCredentials string
	OfferedReplicas     []string
	AcceptedReplicas    string
	Reconciled          bool
}

type liveEval struct {
	Kind    string
	AgentID string
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

const (
	liveEvalExpiry   = "expiry"
	liveEvalDeadline = "deadline"
	liveEvalRollout  = "rollout"
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

	product        *journal.Projection
	durableIndexes map[string]int64
	liveIndex      uint64
	authorityEpoch uint64
	watchers       []*liveWatcher
	allocSync      *allocSync

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
		product:        journal.NewProjection(journal.DurableState{}),
		allocSync:      newAllocSync(),
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
	_, err := d.evaluateObservedDeployment(ctx, allocationID)
	return err
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
		defer func() {
			d.live.resign()
		}()
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

func (l *Live) become(ctx context.Context, readState func(context.Context, func(*sql.Tx, *journal.Projection) error) error, readEpoch func(context.Context) (uint64, error)) error {
	l.mu.Lock()
	l.resetLocked()
	l.publishing = false
	l.accepting = false
	l.mu.Unlock()

	var product *journal.Projection
	if err := readState(ctx, func(_ *sql.Tx, state *journal.Projection) error {
		product = state
		return nil
	}); err != nil {
		l.mu.Lock()
		l.resetLocked()
		l.mu.Unlock()
		return err
	}
	var epoch uint64
	if readEpoch != nil {
		var err error
		epoch, err = readEpoch(ctx)
		if err != nil {
			l.mu.Lock()
			l.resetLocked()
			l.mu.Unlock()
			return err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyProductLocked(journal.Applied{Projection: product, Reset: true})
	durable := l.product.DurableState
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
	l.allocSync.reset()
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
	l.authorityEpoch = 0
	l.product = journal.NewProjection(journal.DurableState{})
	l.durableIndexes = make(map[string]int64)
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

// ObservationOutcome describes how a recorded observation affects downstream
// work and status subscribers.
type ObservationOutcome struct {
	// Changed reports changes requiring rollout work. It implies StatusInvalidated.
	Changed bool
	// StatusInvalidated reports rendered status changed and subscribers must
	// refetch, even when no rollout work is needed.
	StatusInvalidated bool
}

type SyncVersions struct {
	Cursor      int64
	NodeConfig  string
	Credentials string
	Replicas    string
}

// offerHistoryLimit bounds each offered-version history: a cumulative ack can
// lag several in-flight batches and must still match a version offered in
// this session; a larger lag fails validation and reconnects.
const offerHistoryLimit = 64

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

func (s *persistence) readAuthorityEpoch(ctx context.Context) (uint64, error) {
	var epoch uint64
	err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch)
	return epoch, err
}
