//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
)

func TestPruneSourceArchivesDeletesExpiredUnreferencedObjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-old"); err != nil {
		t.Fatal(err)
	}
	var snapshotID, objectKey string
	if err := store.db.QueryRowContext(ctx,
		`SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-old'`,
	).Scan(&snapshotID, &objectKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE source_snapshots SET created_at = $1 WHERE id = $2`,
		time.Now().UTC().AddDate(0, 0, -60), snapshotID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE source_archive_objects SET updated_at = $1 WHERE object_key = $2`,
		time.Now().UTC().AddDate(0, 0, -60), objectKey,
	); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.source.PruneSourceArchives(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted objects = %d, want 1", deleted)
	}
	if _, err := store.source.SourceSnapshotByID(ctx, snapshotID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("snapshot lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.source.Archives().Stat(ctx, objectKey); !source.IsArchiveNotFound(err) {
		t.Fatalf("object lookup error = %v, want archive not found", err)
	}
	var objectRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_archive_objects WHERE object_key = $1`, objectKey).Scan(&objectRows); err != nil {
		t.Fatal(err)
	}
	if objectRows != 0 {
		t.Fatalf("object rows = %d, want 0", objectRows)
	}
}

func TestSourceArchiveObjectsConvergeOnDuplicateDigest(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	archive := []byte("duplicate-source-archive")
	digest, key, err := store.source.StoreSourceArchive(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	digest2, key2, err := store.source.StoreSourceArchive(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	if digest != digest2 || key != key2 {
		t.Fatalf("duplicate store diverged: %s/%s vs %s/%s", digest, key, digest2, key2)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_archive_objects WHERE object_key = $1`, key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("object rows = %d, want 1", rows)
	}
}

func TestPruneSourceArchivesKeepsReferencedAndRecentObjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-recent"); err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-active"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-active")
	if err != nil {
		t.Fatal(err)
	}
	if build.State != deliverycore.BuildStateQueued {
		t.Fatalf("build state = %q", build.State)
	}
	old := time.Now().UTC().AddDate(0, 0, -60)
	if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET created_at = $1 WHERE commit_sha = 'commit-active'`, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	deleted, err := store.source.PruneSourceArchives(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted objects = %d, want 0", deleted)
	}
	var snapshots, objects int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_archive_objects`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 || objects != 2 {
		t.Fatalf("snapshots = %d objects = %d, want 2 and 2", snapshots, objects)
	}
}

func TestPruneSourceArchivesDeletionRaceCollectsOnce(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-race"); err != nil {
		t.Fatal(err)
	}
	var snapshotID, objectKey string
	if err := store.db.QueryRowContext(ctx, `SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-race'`).Scan(&snapshotID, &objectKey); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60)
	if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET created_at = $1 WHERE id = $2`, old, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE source_archive_objects SET updated_at = $1 WHERE object_key = $2`, old, objectKey); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = store.source.PruneSourceArchives(ctx, cutoff)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prune %d: %v", i, err)
		}
	}
	if total := results[0] + results[1]; total != 1 {
		t.Fatalf("total collected = %d, want 1", total)
	}
}

func TestOpenSnapshotArchiveMissingAndStaleObjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-missing"); err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-stale"); err != nil {
		t.Fatal(err)
	}
	var missingID, missingKey, staleID, staleKey string
	if err := store.db.QueryRowContext(ctx, `SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-missing'`).Scan(&missingID, &missingKey); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-stale'`).Scan(&staleID, &staleKey); err != nil {
		t.Fatal(err)
	}
	if err := store.source.Archives().Delete(ctx, missingKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.source.OpenSnapshotArchive(ctx, missingID); !errors.Is(err, source.ErrSnapshotMissing) {
		t.Fatalf("missing object error = %v, want snapshot missing", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET archive_size_bytes = archive_size_bytes + 1 WHERE id = $1`, staleID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.source.OpenSnapshotArchive(ctx, staleID); !errors.Is(err, source.ErrSnapshotCorrupt) {
		t.Fatalf("stale object error = %v, want snapshot corrupt", err)
	}
}

func TestOpenSnapshotArchiveMapsS3BackendFailures(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-s3"); err != nil {
		t.Fatal(err)
	}
	var snapshotID string
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM source_snapshots WHERE commit_sha = 'commit-s3'`).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(credPath, []byte(`{"access_key_id":"test","secret_access_key":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	useBackend := func(status int) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		backend, err := source.NewS3ArchiveStore(config.SourceArchiveS3Config{
			Endpoint:              server.URL,
			Region:                "us-east-1",
			Bucket:                "test-bucket",
			CredentialsFile:       credPath,
			RequestTimeoutSeconds: 5,
			MaxRetries:            1,
		})
		if err != nil {
			t.Fatal(err)
		}
		store.source.ConfigureSourceArchives(backend)
	}
	useBackend(http.StatusServiceUnavailable)
	if _, _, err := store.source.OpenSnapshotArchive(ctx, snapshotID); !errors.Is(err, source.ErrSnapshotTransient) {
		t.Fatalf("unavailable backend error = %v, want transient", err)
	}
	useBackend(http.StatusNotFound)
	if _, _, err := store.source.OpenSnapshotArchive(ctx, snapshotID); !errors.Is(err, source.ErrSnapshotMissing) {
		t.Fatalf("missing backend error = %v, want missing", err)
	}
}

func TestPruneSourceArchivesHealsStrandedDeletingRows(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-stranded-ref"); err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-stranded-free"); err != nil {
		t.Fatal(err)
	}
	var refSnapshotID, refKey, freeSnapshotID, freeKey string
	if err := store.db.QueryRowContext(ctx, `SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-stranded-ref'`).Scan(&refSnapshotID, &refKey); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT id, object_key FROM source_snapshots WHERE commit_sha = 'commit-stranded-free'`).Scan(&freeSnapshotID, &freeKey); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60)
	// Simulate a prune that crashed between the claim and the per-key
	// collection: both rows sit in deleting state with old timestamps.
	for _, key := range []string{refKey, freeKey} {
		if _, err := store.db.ExecContext(ctx,
			`UPDATE source_archive_objects SET state = 'deleting', updated_at = $1 WHERE object_key = $2`,
			old, key,
		); err != nil {
			t.Fatal(err)
		}
	}
	// The free snapshot expires so only its stranded row is collected; the
	// referenced snapshot stays live so its claim must be restored.
	if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET created_at = $1 WHERE id = $2`, old, freeSnapshotID); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.source.PruneSourceArchives(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted objects = %d, want 1", deleted)
	}
	var refState string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM source_archive_objects WHERE object_key = $1`, refKey).Scan(&refState); err != nil {
		t.Fatal(err)
	}
	if refState != source.ArchiveObjectStateReady {
		t.Fatalf("referenced object state = %q, want %q", refState, source.ArchiveObjectStateReady)
	}
	if _, err := store.source.Archives().Stat(ctx, refKey); err != nil {
		t.Fatalf("referenced object stat: %v", err)
	}
	var freeRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_archive_objects WHERE object_key = $1`, freeKey).Scan(&freeRows); err != nil {
		t.Fatal(err)
	}
	if freeRows != 0 {
		t.Fatalf("freed object rows = %d, want 0", freeRows)
	}
	if _, err := store.source.Archives().Stat(ctx, freeKey); !source.IsArchiveNotFound(err) {
		t.Fatalf("freed object lookup error = %v, want archive not found", err)
	}
}

type deleteHookArchiveStore struct {
	source.ArchiveStore
	onDelete func(ctx context.Context, key string) error
}

func (s deleteHookArchiveStore) Delete(ctx context.Context, key string) error {
	return s.onDelete(ctx, key)
}

func TestPruneSourceArchivesSerializesRacingStoreBehindCollection(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	archive := []byte("racing-source-archive")
	_, key, err := store.source.StoreSourceArchive(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60)
	if _, err := store.db.ExecContext(ctx, `UPDATE source_archive_objects SET updated_at = $1 WHERE object_key = $2`, old, key); err != nil {
		t.Fatal(err)
	}
	backend := store.source.Archives()
	deleteStarted := make(chan struct{})
	var deleteOnce sync.Once
	store.source.ConfigureSourceArchives(deleteHookArchiveStore{
		ArchiveStore: backend,
		onDelete: func(ctx context.Context, deleteKey string) error {
			deleteOnce.Do(func() { close(deleteStarted) })
			// Hold the collection transaction open so the racing store
			// must block on the row lock if the lock is held across the
			// object delete, and completes immediately if it is not.
			time.Sleep(500 * time.Millisecond)
			return backend.Delete(ctx, deleteKey)
		},
	})
	type storeResult struct {
		elapsed time.Duration
		err     error
	}
	racerDone := make(chan storeResult, 1)
	go func() {
		<-deleteStarted
		start := time.Now()
		_, _, err := store.source.StoreSourceArchive(ctx, archive)
		racerDone <- storeResult{elapsed: time.Since(start), err: err}
	}()
	deleted, err := store.source.PruneSourceArchives(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted objects = %d, want 1", deleted)
	}
	var racer storeResult
	select {
	case racer = <-racerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("racing store did not finish")
	}
	if racer.err != nil {
		t.Fatalf("racing store: %v", racer.err)
	}
	// The racer started while collection held the row lock, so it must have
	// waited out the 500ms delete hook before its upsert could proceed.
	if racer.elapsed < 400*time.Millisecond {
		t.Fatalf("racing store finished in %v, want it to block behind collection", racer.elapsed)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM source_archive_objects WHERE object_key = $1`, key).Scan(&state); err != nil {
		t.Fatalf("object row lookup: %v", err)
	}
	if state != source.ArchiveObjectStateReady {
		t.Fatalf("object state = %q, want %q", state, source.ArchiveObjectStateReady)
	}
	if _, err := backend.Stat(ctx, key); err != nil {
		t.Fatalf("re-stored object stat: %v", err)
	}
}
