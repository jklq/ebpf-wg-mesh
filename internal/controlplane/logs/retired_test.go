package logs

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRetiredGenerationJournalIsRecoveredWithoutTakingLiveOwnership(t *testing.T) {
	ctx := context.Background()
	store := &fakeFlushStore{enabled: true}
	root := t.TempDir()
	old := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: filepath.Join(root, "old")})
	if old.EnqueueLines([]LogLineInput{{ID: "acknowledged", ServiceID: "svc", ProjectID: "p", Line: "preserved"}}) != AdmitAccepted {
		t.Fatal("journal rejected")
	}
	own := filepath.Join(root, "new")
	if err := ReplayRetiredJournals(ctx, store, own, 0); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	count := len(store.lines)
	store.mu.Unlock()
	if count != 0 {
		t.Fatal("journal with a live owner was stolen")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReplayRetiredJournals(ctx, store, own, 0); err != nil {
		t.Fatal(err)
	}
	if err := ReplayRetiredJournals(ctx, store, own, 0); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 1 || store.lines[0].ID != "acknowledged" {
		t.Fatal("retired acknowledged records were lost or replayed", store.lines)
	}
}
