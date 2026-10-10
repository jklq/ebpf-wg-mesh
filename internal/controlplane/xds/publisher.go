package xds

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Inputs is the live control-plane state that one snapshot renders.
type Inputs struct {
	Backends     []Backend
	Certificates []Certificate
	Challenges   []Challenge
}

type SnapshotSource interface {
	IngressInputs(context.Context) (Inputs, error)
	WithLeaseGuard(context.Context, func() error) error
}

type Publication struct {
	Version   string
	Inputs    []byte
	Publisher string
}

type PublicationStore interface {
	LoadPublication(context.Context) (Publication, error)
	CompareAndSwapPublication(ctx context.Context, oldVersion string, pub Publication) (bool, error)
}

type NodeObservation struct {
	NodeID         string
	AppliedVersion string
	NACKs          int64
	LastNACK       string
}

type NodeStore interface {
	NodeActive(ctx context.Context, nodeID string) (bool, error)
	UpsertNodeObservations(ctx context.Context, observations []NodeObservation) error
	ListNodeObservations(ctx context.Context) ([]NodeObservation, error)
}

const defaultPublishMinSyncInterval = 2 * time.Second

// Publisher recomputes the xDS snapshot from control-plane state and serves it on the
// attached Server. It implements the delivery.PlatformIngress contract.
type Publisher struct {
	source    SnapshotSource
	pubs      PublicationStore
	nodes     NodeStore
	server    *Server
	keys      KeyPairSource
	static    []StaticRoute
	http      []string
	https     []string
	publisher string
	minSync   time.Duration
	pushMu    sync.Mutex
	requestCh chan struct{}
}

type PublisherConfig struct {
	Source           SnapshotSource
	Publications     PublicationStore
	Nodes            NodeStore
	Server           *Server
	Keys             KeyPairSource
	Static           []StaticRoute
	HTTPListenAddrs  []string
	HTTPSListenAddrs []string
	PublisherID      string
	MinSync          time.Duration
}

// NewPublisher builds a Publisher. A nil Server disables local serving; a nil PublicationStore
// disables the durable row; a nil NodeStore disables node tracking (Converged is then true).
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
		keys:      cfg.Keys,
		static:    append([]StaticRoute(nil), cfg.Static...),
		http:      append([]string(nil), cfg.HTTPListenAddrs...),
		https:     append([]string(nil), cfg.HTTPSListenAddrs...),
		publisher: cfg.PublisherID,
		minSync:   minSync,
		requestCh: make(chan struct{}, 1),
	}
}

// Sync recomputes and publishes the snapshot; a build failure retains last-known-good.
func (p *Publisher) Sync(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pushMu.Lock()
	defer p.pushMu.Unlock()

	return p.source.WithLeaseGuard(ctx, func() error {
		inputs, err := p.source.IngressInputs(ctx)
		if err != nil {
			return err
		}
		snap, err := Build(ctx, BuildInput{
			Backends:         inputs.Backends,
			Static:           p.static,
			HTTPListenAddrs:  p.http,
			HTTPSListenAddrs: p.https,
			Certificates:     inputs.Certificates,
			Challenges:       inputs.Challenges,
			Keys:             p.keys,
		})
		if err != nil {
			return err
		}
		return p.publishLocked(ctx, snap)
	})
}

func (p *Publisher) RequestSync() {
	if p == nil {
		return
	}
	select {
	case p.requestCh <- struct{}{}:
	default:
	}
}

// Run republishes on request and on a floor interval until ctx ends.
func (p *Publisher) Run(ctx context.Context) error {
	return p.loop(ctx, p.requestCh, func() {
		if err := p.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("xds publish failed", "error", err)
		}
	})
}

// Follow tracks the durable publication and node apply state on every replica.
func (p *Publisher) Follow(ctx context.Context) error {
	return p.loop(ctx, nil, func() {
		if err := p.Replicate(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("xds publication follow failed", "error", err)
		}
	})
}

func (p *Publisher) loop(ctx context.Context, requestCh <-chan struct{}, tick func()) error {
	if p == nil {
		<-ctx.Done()
		return nil
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
		case <-requestCh:
			tick()
		}
	}
}

// Converged gates destroying withdrawn allocations: every active Envoy must fully apply first.
func (p *Publisher) Converged(ctx context.Context) (bool, error) {
	_, converged, err := p.Applied(ctx)
	return converged, err
}

// Applied reports how many Envoy nodes are active and whether each fully applied
// the served version. With no nodes the snapshot counts as applied.
func (p *Publisher) Applied(ctx context.Context) (nodes int, converged bool, err error) {
	if p == nil || p.nodes == nil {
		return 0, true, nil
	}
	version := ""
	if p.server != nil {
		if status := p.server.Status(); status.HasSnapshot {
			version = status.Version
		}
	}
	if version == "" && p.pubs != nil {
		pub, err := p.pubs.LoadPublication(ctx)
		if err != nil {
			return 0, false, err
		}
		version = pub.Version
	}
	if version == "" {
		return 0, true, nil
	}
	if err := p.flushNodeObservations(ctx); err != nil {
		return 0, false, err
	}
	observations, err := p.nodes.ListNodeObservations(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, node := range observations {
		if node.AppliedVersion != version {
			return len(observations), false, nil
		}
	}
	return len(observations), true, nil
}

// Replicate adopts the durable publication and flushes node apply state once.
func (p *Publisher) Replicate(ctx context.Context) error {
	if err := p.Refresh(ctx); err != nil {
		return err
	}
	return p.flushNodeObservations(ctx)
}

// Refresh adopts the durable publication into the local server. It is serialized with Sync
// and rechecks the row after building, so an adoption never serves a stale publication.
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

func (p *Publisher) adoptPublicationLocked(ctx context.Context) (done bool, err error) {
	pub, err := p.pubs.LoadPublication(ctx)
	if err != nil {
		return false, err
	}
	if pub.Version == "" {
		return true, nil
	}
	if status := p.server.Status(); status.HasSnapshot && status.Version == pub.Version {
		return true, nil
	}
	snap, err := BuildFromInputs(ctx, pub.Inputs, p.keys)
	if err != nil {
		return false, fmt.Errorf("published snapshot unusable: %w", err)
	}
	if snap.Version != pub.Version {
		return false, fmt.Errorf("published inputs version %s does not match row version %s", snap.Version, pub.Version)
	}
	recheck, err := p.pubs.LoadPublication(ctx)
	if err != nil {
		return false, err
	}
	if recheck.Version != snap.Version {
		return false, nil
	}
	if status := p.server.Status(); status.HasSnapshot && status.Version == snap.Version {
		return true, nil
	}
	p.server.Publish(ctx, snap)
	return true, nil
}

// flushNodeObservations persists what this replica's subscribers applied. A node reports its
// version only when fully applied; otherwise the store keeps the previous version.
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
			NodeID: nodeID, AppliedVersion: applied, NACKs: node.NACKs, LastNACK: node.LastNACK,
		})
	}
	if len(observations) == 0 {
		return nil
	}
	return p.nodes.UpsertNodeObservations(ctx, observations)
}

func (p *Publisher) publishLocked(ctx context.Context, snap *Snapshot) error {
	if p.pubs != nil {
		accepted := false
		for range 2 {
			pub, err := p.pubs.LoadPublication(ctx)
			if err != nil {
				return err
			}
			if pub.Version == snap.Version {
				accepted = true
				break
			}
			won, err := p.pubs.CompareAndSwapPublication(ctx, pub.Version, Publication{
				Version: snap.Version, Inputs: snap.Inputs, Publisher: p.publisher,
			})
			if err != nil {
				return err
			}
			if won {
				accepted = true
				break
			}
		}
		if !accepted {
			return fmt.Errorf("publication changed during publish")
		}
	}
	if p.server != nil {
		p.server.Publish(ctx, snap)
	}
	return nil
}
