package xds

import (
	"context"
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

// Publication is the last version row written by the live owner.
type Publication struct {
	Version   string
	Hash      string
	Publisher string
}

// PublicationStore persists the published version so a takeover observes
// what the previous owner published and racing owners converge instead of
// flapping. CompareAndSwapPublication writes only when the stored hash still
// equals oldHash (empty matches an absent row) and reports whether it won.
type PublicationStore interface {
	LoadPublication(context.Context) (Publication, error)
	CompareAndSwapPublication(ctx context.Context, oldHash, version, hash string, counts Counts, publisher string) (bool, error)
}

const defaultPublishMinSyncInterval = 2 * time.Second

// Publisher recomputes the xDS snapshot from control-plane state and serves
// it on the attached Server. It implements the delivery.PlatformIngress
// contract (Sync plus RequestSync), so every mutation that used to push Caddy
// config now republishes xDS.
type Publisher struct {
	source    SnapshotSource
	pubs      PublicationStore
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
	Server       *Server
	Static       []StaticRoute
	ListenAddrs  []string
	PublisherID  string
	MinSync      time.Duration
}

// NewPublisher builds a Publisher. A nil Server disables local serving (the
// publication row is still maintained); a nil PublicationStore disables the
// row (single-replica use).
func NewPublisher(cfg PublisherConfig) *Publisher {
	minSync := cfg.MinSync
	if minSync <= 0 {
		minSync = defaultPublishMinSyncInterval
	}
	return &Publisher{
		source:    cfg.Source,
		pubs:      cfg.Publications,
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
			won, err := p.pubs.CompareAndSwapPublication(ctx, pub.Hash, snap.Version, snap.Hash, snap.Counts, p.publisher)
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
