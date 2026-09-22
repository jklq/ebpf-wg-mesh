package xds

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	backends []Backend
	calls    atomic.Int32
	guardErr error
	// firstStarted is closed when the first read starts; the read then
	// waits for blockFirst to close so bursts queue behind it.
	firstStarted chan struct{}
	blockFirst   chan struct{}
}

func (f *fakeSource) HealthyIngressBackends(context.Context) ([]Backend, error) {
	if f.calls.Add(1) == 1 && f.blockFirst != nil {
		close(f.firstStarted)
		<-f.blockFirst
	}
	return append([]Backend(nil), f.backends...), nil
}

func (f *fakeSource) WithLeaseGuard(_ context.Context, fn func() error) error {
	if f.guardErr != nil {
		return f.guardErr
	}
	return fn()
}

type fakePublications struct {
	mu     sync.Mutex
	pub    Publication
	writes int
}

func (f *fakePublications) LoadPublication(context.Context) (Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pub, nil
}

func (f *fakePublications) CompareAndSwapPublication(_ context.Context, oldHash, version, hash string, _ Counts, publisher string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pub.Hash != oldHash {
		return false, nil
	}
	f.pub = Publication{Version: version, Hash: hash, Publisher: publisher}
	f.writes++
	return true, nil
}

func testPublisher(source *fakeSource, pubs *fakePublications) (*Publisher, *Server) {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel
	server := NewServer(ctx)
	publisher := NewPublisher(PublisherConfig{
		Source:       source,
		Publications: pubs,
		Server:       server,
		ListenAddrs:  []string{":8080"},
		PublisherID:  "replica-a",
	})
	return publisher, server
}

func TestPublisherPublishesOncePerContent(t *testing.T) {
	t.Parallel()

	source := &fakeSource{backends: []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}}
	pubs := &fakePublications{}
	publisher, server := testPublisher(source, pubs)

	ctx := context.Background()
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	first := server.Status()
	if !first.HasSnapshot {
		t.Fatal("expected a published snapshot")
	}
	if pubs.writes != 1 {
		t.Fatalf("writes = %d, want 1", pubs.writes)
	}

	// Unchanged content republishes locally but does not rewrite the row.
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pubs.writes != 1 {
		t.Fatalf("writes = %d, want 1 after unchanged sync", pubs.writes)
	}
	if got := server.Status().Version; got != first.Version {
		t.Fatalf("version = %s, want %s", got, first.Version)
	}

	// Changed content publishes a new version.
	source.backends = append(source.backends, Backend{Domain: "b.example.com", Upstream: "10.0.0.11:8080"})
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := server.Status().Version; got == first.Version {
		t.Fatal("expected a new version after the backend change")
	}
	if pubs.writes != 2 {
		t.Fatalf("writes = %d, want 2", pubs.writes)
	}
}

func TestPublisherAdoptsRacingWinner(t *testing.T) {
	t.Parallel()

	// Another owner already published this exact content (deterministic
	// bytes): adopt it without rewriting the row.
	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	snap, err := Build(BuildInput{Backends: backends, ListenAddrs: []string{":8080"}})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{backends: backends}
	pubs := &fakePublications{pub: Publication{Version: snap.Version, Hash: snap.Hash, Publisher: "replica-b"}}
	publisher, server := testPublisher(source, pubs)

	if err := publisher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pubs.writes != 0 {
		t.Fatalf("writes = %d, want 0 (adopt, not rewrite)", pubs.writes)
	}
	if got := server.Status().Version; got != snap.Version {
		t.Fatalf("served version = %s, want %s", got, snap.Version)
	}
}

func TestPublisherFencedAfterTakeover(t *testing.T) {
	t.Parallel()

	source := &fakeSource{
		backends: []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		guardErr: errors.New("lease lost"),
	}
	pubs := &fakePublications{}
	publisher, server := testPublisher(source, pubs)

	if err := publisher.Sync(context.Background()); err == nil {
		t.Fatal("expected a lease-guard error from a stale owner")
	}
	if server.Status().HasSnapshot {
		t.Fatal("a fenced owner must not serve a snapshot")
	}
	if pubs.writes != 0 {
		t.Fatal("a fenced owner must not write the publication row")
	}
}

func TestPublisherRequestSyncCoalescesBurst(t *testing.T) {
	t.Parallel()

	source := &fakeSource{firstStarted: make(chan struct{}), blockFirst: make(chan struct{})}
	pubs := &fakePublications{}
	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(ctx)
	publisher := NewPublisher(PublisherConfig{
		Source:       source,
		Publications: pubs,
		Server:       server,
		ListenAddrs:  []string{":8080"},
		PublisherID:  "replica-a",
		MinSync:      time.Hour,
	})

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- publisher.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		<-runDone
		cancel()
	})

	deadline := time.Now().Add(5 * time.Second)
	select {
	case <-source.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial sync never started")
	}
	// The initial sync is blocked, so the whole burst must coalesce into
	// the single queued request.
	publisher.RequestSync()
	publisher.RequestSync()
	publisher.RequestSync()
	close(source.blockFirst)
	for source.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := source.calls.Load(); got != 2 {
		t.Fatalf("backend reads = %d, want exactly 2 (initial plus one coalesced)", got)
	}
}
