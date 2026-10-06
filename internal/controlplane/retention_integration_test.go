//go:build integration

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
)

func TestSourceRetentionKeepsAutomaticRetriesAndDropsTerminalArchives(t *testing.T) {
	t.Parallel()
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "retention-source"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "retention-source")
	if err != nil {
		t.Fatal(err)
	}
	var key string
	if err := store.db.QueryRowContext(ctx, `SELECT object_key FROM source_snapshots WHERE id = $1`, build.SourceSnapshotID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	prune := func() int {
		t.Helper()
		count, err := store.source.PruneSourceArchives(ctx, time.Now().UTC().Add(10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	if prune() != 0 {
		t.Fatal("queued build lost its source")
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if prune() != 0 {
		t.Fatal("running build lost its source")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs SET lease_expires_at = $1 WHERE id = $2`, time.Now().Add(-time.Minute), build.ID); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatal(err)
	}
	if prune() != 0 {
		t.Fatal("automatic retry lost its source")
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, build.CommitSHA, "", "cannot compile"); err != nil {
		t.Fatal(err)
	}
	if count := prune(); count != 1 {
		t.Fatalf("terminal build collected %d source objects, want 1", count)
	}
	if _, err := store.source.Archives().Stat(ctx, key); !source.IsArchiveNotFound(err) {
		t.Fatalf("source bytes survived completion: %v", err)
	}
	if _, err := store.source.SourceRevisionByIDTx(ctx, store.db, build.SourceRevisionID); err != nil {
		t.Fatalf("lost commit metadata: %v", err)
	}
}

func TestHistoricalRollbackRefetchesExactCommitOrFailsHelpfully(t *testing.T) {
	for _, commit := range []string{"commit-public-main", "commit-removed-from-github"} {
		t.Run(commit, func(t *testing.T) {
			t.Parallel()
			store := openTestStore(t)
			ctx := context.Background()
			projectID, serviceID := createRepoBackedTestService(t, store, ctx, "public/hello", 0, "main")
			server := newTestGitHubServer(t, nil)
			client, err := NewGitHubClient(server.config())
			if err != nil {
				t.Fatal(err)
			}
			catalog := NewGitHubCatalog(store.source, client)
			linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
			service, err := testDelivery(store).ServiceByID(ctx, testUser("user-1"), serviceID)
			if err != nil {
				t.Fatal(err)
			}
			buildVersion := func(sha, image string) deliverycore.DeploymentRecord {
				t.Helper()
				if err := seedReadySourceState(t, store, service, sha); err != nil {
					t.Fatal(err)
				}
				build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, sha)
				if err != nil {
					t.Fatal(err)
				}
				claimBuildForTest(t, store, ctx, "builder-1", build.ID)
				if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, sha, image, ""); err != nil {
					t.Fatal(err)
				}
				completeActionRollout(t, store, service.ID)
				return currentDeploymentForTest(t, store, ctx, service.ID)
			}
			old := buildVersion(commit, pinnedImage("a"))
			current := buildVersion("commit-new", pinnedImage("b"))
			if _, err := store.source.PruneSourceArchives(ctx, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := testDelivery(store).PruneBuildArtifacts(ctx, time.Now().Add(25*time.Hour)); err != nil {
				t.Fatal(err)
			}
			assertRetainedImage(t, store, ctx, old.ArtifactID, false)
			assertRetainedImage(t, store, ctx, current.ArtifactID, true)
			_, action, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, old.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "historical-rebuild", "")
			if err != nil {
				t.Fatal(err)
			}
			delivery := testDelivery(store).Delivery
			coordinator := NewGitHubCoordinator(store.source, store.source.Work(), delivery, catalog, client, 5*time.Minute)
			reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
			drainSourceWork(t, reconciler, ctx)
			result := currentDeploymentForTest(t, store, ctx, service.ID)
			if commit == "commit-removed-from-github" {
				if result.State != deliverycore.DeploymentStateFailed || !strings.Contains(result.Detail, commit) || !strings.Contains(result.Detail, "check repository access") {
					t.Fatalf("unhelpful refetch failure: %+v", result)
				}
				if !strings.Contains(result.Detail, "source archives are removed") {
					t.Fatalf("missing retention context: %s", result.Detail)
				}
				if _, _, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, result.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, "retry-refetch", ""); err != nil {
					t.Fatal(err)
				}
				drainSourceWork(t, reconciler, ctx)
				retried := currentDeploymentForTest(t, store, ctx, service.ID)
				if retried.State != deliverycore.DeploymentStateFailed || !strings.Contains(retried.Detail, commit) {
					t.Fatalf("refetch retry changed historical commit: %+v", retried)
				}
				return
			}
			if result.State != deliverycore.DeploymentStateQueuedBuild || result.BuildID == "" {
				t.Fatalf("historical build not queued: %+v", result)
			}
			build, err := delivery.BuildByID(ctx, result.BuildID)
			if err != nil {
				t.Fatal(err)
			}
			if build.CommitSHA != commit {
				t.Fatalf("rollback fetched branch head %q instead of historical %q", build.CommitSHA, commit)
			}
			var head string
			if err := store.db.QueryRowContext(ctx, `SELECT head_commit_sha FROM source_bindings WHERE service_id = $1`, service.ID).Scan(&head); err != nil {
				t.Fatal(err)
			}
			if head != "commit-new" {
				t.Fatalf("rollback regressed the observed branch head: %s", head)
			}
			if _, same, err := applyDeploymentActionForTest(ctx, store, "user-1", service.ID, old.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "historical-rebuild", ""); err != nil || same.ResultDeploymentID != result.ID {
				t.Fatalf("idempotent rollback result: %+v, %v (initial %s)", same, err, action.ResultDeploymentID)
			}
			claimBuildForTest(t, store, ctx, "builder-1", build.ID)
			if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, commit, pinnedImage("c"), ""); err != nil {
				t.Fatal(err)
			}
			completeActionRollout(t, store, service.ID)
		})
	}
}

type retentionDeleteProbe struct {
	err     error
	deleted []string
}

func (p *retentionDeleteProbe) PrepareDelete(_ context.Context, ref string) (func(context.Context) error, error) {
	return func(context.Context) error { p.deleted = append(p.deleted, ref); return p.err }, nil
}

func TestImageDeletionRetriesAndPreservesSharedCurrentImages(t *testing.T) {
	t.Parallel()
	store, ctx, _, _, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	current := currentDeploymentForTest(t, store, ctx, service.ID)
	for _, ref := range []string{current.ImageDigest, pinnedImage("b")} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO registry_image_deletions(image_ref, created_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, ref, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	p := &retentionDeleteProbe{err: errors.New("registry unavailable")}
	d := testDelivery(store).Delivery
	d.SetImageDeleter(p)
	if _, err := d.CollectExpiredImages(ctx); err == nil {
		t.Fatal("deletion failure was lost")
	}
	p.err = nil
	if count, err := d.CollectExpiredImages(ctx); err != nil || count != 1 {
		t.Fatalf("deletion retry: count %d, %v", count, err)
	}
	for _, ref := range p.deleted {
		if ref == current.ImageDigest {
			t.Fatal("deleted a shared current image")
		}
	}
	if count, err := d.CollectExpiredImages(ctx); err != nil || count != 0 {
		t.Fatalf("deletion was not idempotent: %d, %v", count, err)
	}
}
