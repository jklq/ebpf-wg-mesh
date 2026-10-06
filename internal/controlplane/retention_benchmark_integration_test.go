//go:build integration

package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// This probe measures the real SQLStore/file backend cleanup after twenty
// completed builds. Build execution is simulated; cleanup and storage are real.
func TestSourceRetentionStorageProbe(t *testing.T) {
	if os.Getenv("RETENTION_STORAGE_PROBE") != "1" {
		t.Skip("set RETENTION_STORAGE_PROBE=1 for the storage benchmark")
	}
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	for version := range 20 {
		commit := fmt.Sprintf("probe-%d", version)
		if err := seedReadySourceState(t, store, service, commit); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 1<<20)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		archive := dockerfileMarkerArchive(hex.EncodeToString(payload))
		digest, key, err := storeTestArchive(ctx, store, archive)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET digest = $1, object_key = $2, archive_size_bytes = $3 WHERE commit_sha = $4`, digest, key, len(archive), commit); err != nil {
			t.Fatal(err)
		}
		build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, commit)
		if err != nil {
			t.Fatal(err)
		}
		claimBuildForTest(t, store, ctx, "builder-1", build.ID)
		if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, commit, "", "probe completed"); err != nil {
			t.Fatal(err)
		}
	}
	var before, after int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM source_archive_objects`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	// Advance past the five-minute staging protection; no active build remains.
	collected, err := store.source.PruneSourceArchives(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM source_archive_objects`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("completed build source bytes remain: %d", after)
	}
	result, err := json.Marshal(map[string]any{"completed_builds": 20, "archive_bytes_before": before,
		"archive_bytes_after": after, "collected_objects": collected, "cleanup_seconds": elapsed.Seconds()})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(result))
}
