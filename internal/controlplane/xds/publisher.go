package xds

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SnapshotSource isolates the publisher from the control-plane store: reads
// are plain queries, publication is fenced by the singleton lease.
type SnapshotSource interface {
	HealthyIngressBackends(context.Context) ([]Backend, error)
	WithLeaseGuard(context.Context, func() error) error
}

// Publication is the last version row written by the live owner. Inputs are
// the canonical hash preimage: any replica can rebuild the exact snapshot
// from them and serve it, which is what makes the xDS endpoint shared across
// replicas instead of pinned to one owner's memory.
type Publication struct {
	Version   string
	Hash      string
	Inputs    []byte
	Counts    Counts
	Publisher string
}

// PublicationStore persists the published version so a takeover observes
// what the previous owner published and racing owners converge instead of
// flapping. CompareAndSwapPublication writes only when the stored hash still
// equals oldHash (empty matches an absent row) and reports whether it won.
type PublicationStore interface {
	LoadPublication(context.Context) (Publication, error)
	CompareAndSwapPublication(ctx context.Context, oldHash string, pub Publication) (bool, error)
}

// NodeObservation is one Envoy's durable apply state. AppliedHash is the
// content hash the node has fully applied ("" until then). NACKs and
// LastNACK retain the latest rejection for operators.
type NodeObservation struct {
	NodeID      string
	AppliedHash string
	NACKs       int64
	LastNACK    string
}

// NodeStore persists per-node apply state across replicas and restarts so
// the live owner can require real convergence before draining a withdrawn
// allocation. Rows are never deleted implicitly: a disconnected Envoy keeps
// serving its last-known-good config, so its withdrawals must wait for the
// ACK to actually arrive. Availability policy for instances that never come
// back is the fleet work.
type NodeStore interface {
	UpsertNodeObservations(ctx context.Context, observations []NodeObservation) error
	ListNodeObservations(ctx context.Context) ([]NodeObservation, error)
}

const defaultPublishMinSyncInterval = 2 * time.Second

// Publisher recomputes the xDS snapshot from control-plane state and serves
// it on the attached Server. It implements the delivery.PlatformIngress
// contract (Sync, RequestSync, Converged), so every mutation that used to
// push Caddy config now republishes xDS and rollouts can wait for applied
// withdrawals before draining.
type Publisher struct {
	source    SnapshotSource
	pubs      PublicationStore
	nodes     NodeStore
	server    *Server
	static    []StaticRoute
	listen    []string
	publisher string
	minSync   time.Duration
	pushMu    sync.Mutex
	requestCh chan struct{}
}

// PublisherConfig wires a Publisher. Static routes and listen addresses come
// from ingress configuration; PublisherID names this replica in the
// publication row.
type PublisherConfig struct {
	Source       SnapshotSource
	Publications PublicationStore
	Nodes        NodeStore
	Server       *Server
	Static       []StaticRoute
	ListenAddrs  []string
	PublisherID  string
	MinSync      time.Duration
}

// NewPublisher builds a Publisher. A nil Server disables local serving (the
// publication row is still maintained); a nil PublicationStore disables the
// row (single-replica use); a nil NodeStore disables durable node tracking
// (Converged then reports true).
func NewPublisher(cfg PublisherConfig) *Publisher {
	minSync := cfg.MinSync
	if minSync <= 0 {
		minSync = defaultPublishMinSyncInterval
	}
	return &Publisher{
		source:    cfg.Source,
		pubs:      cfg.Publications,
		nodes:     cfg.Nodes,
		server:    cfg.Server,
		static:    append([]StaticRoute(nil), cfg.Static...),
		listen:    append([]string(nil), cfg.ListenAddrs...),
		publisher: cfg.PublisherID,
		minSync:   minSync,
		requestCh: make(chan struct{}, 1),
	}
}

// MinSyncInterval reports the coalescing interval, for tests.
func (p *Publisher) MinSyncInterval() time.Duration { return p.minSync }

// Sync recomputes and publishes the snapshot. It is serialized and safe for
// concurrent use; a build failure retains the last-known-good snapshot.
func (p *Publisher) Sync(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pushMu.Lock()
	defer p.pushMu.Unlock()

	backends, err := p.source.HealthyIngressBackends(ctx)
	if err != nil {
		return err
	}
	snap, err := Build(BuildInput{
		Backends:    backends,
		Static:      p.static,
		ListenAddrs: p.listen,
	})
	if err != nil {
		return err
	}
	return p.source.WithLeaseGuard(ctx, func() error {
		return p.publishLocked(ctx, snap)
	})
}

// RequestSync asks the Run loop for an out-of-band republish.
func (p *Publisher) RequestSync() {
	if p == nil {
		return
	}
	select {
	case p.requestCh <- struct{}{}:
	default:
	}
}

// Run republishes on request and on a floor interval until ctx ends. Only
// the live owner runs this; WithLeaseGuard fences stale owners that have
// not observed takeover yet.
func (p *Publisher) Run(ctx context.Context) error {
	if p == nil {
		<-ctx.Done()
		return nil
	}
	syncNow := func() {
		if err := p.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("xds publish failed", "error", err)
		}
	}
	syncNow()
	ticker := time.NewTicker(p.minSync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			syncNow()
		case <-p.requestCh:
			syncNow()
		}
	}
}

// Follow keeps this replica's server on the durable publication and records
// node apply state. It runs on every replica, not just the live owner: after
// a lease takeover or rolling replacement an Envoy must receive the current
// snapshot from whichever xDS endpoint it reaches, and drain gating must see
// ACKs wherever the subscriber landed.
func (p *Publisher) Follow(ctx context.Context) error {
	if p == nil {
		<-ctx.Done()
		return nil
	}
	tick := func() {
		if err := p.Replicate(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("xds publication follow failed", "error", err)
		}
	}
	tick()
	ticker := time.NewTicker(p.minSync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tick()
		}
	}
}

// Converged reports whether every known Envoy has fully applied the current
// publication. Rollouts must not destroy withdrawn allocations before this:
// a disconnected or NACKing Envoy keeps routing to its last-known-good
// endpoints until it applies the withdrawal. Subscribers are registered
// durably at first contact, before they can hold config, so anything that
// can route to a withdrawn endpoint is known and blocks until it applies;
// observed nodes with no fully applied version are mid-apply and block too.
func (p *Publisher) Converged(ctx context.Context) (bool, error) {
	if p == nil || p.nodes == nil {
		return true, nil
	}
	hash := ""
	if p.server != nil {
		if status := p.server.Status(); status.HasSnapshot {
			hash = status.Hash
		}
	}
	if hash == "" && p.pubs != nil {
		pub, err := p.pubs.LoadPublication(ctx)
		if err != nil {
			return false, err
		}
		hash = pub.Hash
	}
	if hash == "" {
		return true, nil
	}
	// Flush first: a locally observed subscriber must be durable before the
	// barrier passes. Cross-replica flush lag can only delay drains (stale
	// hashes block), and first-contact registration happens before any
	// config is served, so nothing untracked can hold routes.
	if err := p.flushNodeObservations(ctx); err != nil {
		return false, err
	}
	nodes, err := p.nodes.ListNodeObservations(ctx)
	if err != nil {
		return false, err
	}
	for _, node := range nodes {
		if node.AppliedHash != hash {
			return false, nil
		}
	}
	return true, nil
}

// Replicate adopts the durable publication and flushes node apply state
// once. Follow runs it on a floor interval.
func (p *Publisher) Replicate(ctx context.Context) error {
	if err := p.Refresh(ctx); err != nil {
		return err
	}
	return p.flushNodeObservations(ctx)
}

// Refresh adopts the durable publication into the local server. It is
// serialized with Sync (a follow tick or first-contact refresh can never
// overwrite newer locally published content) and rechecks the row after
// building, so an adoption never serves a publication the row has already
// moved past.
func (p *Publisher) Refresh(ctx context.Context) error {
	if p == nil || p.pubs == nil || p.server == nil {
		return nil
	}
	p.pushMu.Lock()
	defer p.pushMu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		done, err := p.adoptPublicationLocked(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		lastErr = fmt.Errorf("publication moved during adoption")
	}
	return lastErr
}

// adoptPublicationLocked makes one adoption attempt. done reports that the
// local server serves the durable publication; a moved row retries.
func (p *Publisher) adoptPublicationLocked(ctx context.Context) (done bool, err error) {
	pub, err := p.pubs.LoadPublication(ctx)
	if err != nil {
		return false, err
	}
	if pub.Hash == "" {
		return true, nil
	}
	if status := p.server.Status(); status.HasSnapshot && status.Hash == pub.Hash {
		return true, nil
	}
	snap, err := BuildFromInputs(pub.Inputs)
	if err != nil {
		return false, fmt.Errorf("published snapshot unusable: %w", err)
	}
	if snap.Hash != pub.Hash {
		return false, fmt.Errorf("published inputs hash %s does not match row hash %s", snap.Hash, pub.Hash)
	}
	// The row may have moved while the snapshot rebuilt: publishing the
	// stale build would serve withdrawn configuration from a lagging
	// replica. Retry against the moved row instead.
	recheck, err := p.pubs.LoadPublication(ctx)
	if err != nil {
		return false, err
	}
	if recheck.Hash != snap.Hash {
		return false, nil
	}
	if status := p.server.Status(); status.HasSnapshot && status.Hash == snap.Hash {
		return true, nil
	}
	p.server.Publish(ctx, snap)
	return true, nil
}

// flushNodeObservations persists what this replica's subscribers applied.
// A node fully applied only reports its hash when every required type is at
// the served version; the store keeps the previous hash otherwise, so a
// node mid-apply or NACKing still reports the stale version it may route
// with.
func (p *Publisher) flushNodeObservations(ctx context.Context) error {
	if p.nodes == nil || p.server == nil {
		return nil
	}
	status := p.server.Status()
	if !status.HasSnapshot {
		return nil
	}
	observations := make([]NodeObservation, 0, len(status.Nodes))
	for nodeID, node := range status.Nodes {
		applied := ""
		if node.FullyApplied(status.Version, status.RequiredTypes) {
			applied = status.Version
		}
		observations = append(observations, NodeObservation{
			NodeID: nodeID, AppliedHash: applied, NACKs: node.NACKs, LastNACK: node.LastNACK,
		})
	}
	if len(observations) == 0 {
		return nil
	}
	return p.nodes.UpsertNodeObservations(ctx, observations)
}

// publishLocked records the snapshot in the publication row (compare-and-swap
// so racing owners converge) and serves it locally.
func (p *Publisher) publishLocked(ctx context.Context, snap *Snapshot) error {
	if p.pubs != nil {
		for range 2 {
			pub, err := p.pubs.LoadPublication(ctx)
			if err != nil {
				return err
			}
			if pub.Hash == snap.Hash {
				break
			}
			won, err := p.pubs.CompareAndSwapPublication(ctx, pub.Hash, Publication{
				Version: snap.Version, Hash: snap.Hash, Inputs: snap.Inputs,
				Counts: snap.Counts, Publisher: p.publisher,
			})
			if err != nil {
				return err
			}
			if won {
				break
			}
			// Lost the race: reload. Deterministic bytes mean the winner
			// usually wrote the same hash, which the next iteration adopts.
		}
	}
	if p.server != nil {
		p.server.Publish(ctx, snap)
	}
	return nil
}
