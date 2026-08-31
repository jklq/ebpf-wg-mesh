//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestPruneSourceArchivesDeletesExpiredUnreferencedObjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
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
	deleted, err := store.pruneSourceArchives(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted objects = %d, want 1", deleted)
	}
	if _, err := store.sourceSnapshotByID(ctx, snapshotID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("snapshot lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.sourceArchives.Get(ctx, objectKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object lookup error = %v, want os.ErrNotExist", err)
	}
}
