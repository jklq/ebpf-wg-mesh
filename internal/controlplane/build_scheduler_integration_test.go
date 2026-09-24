//go:build integration

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func createSecondRepoService(t *testing.T, store *persistence, ctx context.Context, projectID string) deliverycore.ServiceRecord {
	t.Helper()
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web-b", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService(web-b): %v", err)
	}
	return service
}

func schedulerTestDelivery(store *persistence, cfg deliverycore.BuildSchedulerConfig) *testDeliveryHarness {
	d := newTestDelivery(store, nil, nil, nil)
	d.SetBuildSchedulerConfigForTest(cfg)
	return d
}

func claimBuildWith(t *testing.T, d *testDeliveryHarness, ctx context.Context, builderID, expectedBuildID string) deliverycore.BuildRunRecord {
	t.Helper()
	claimed, err := d.ClaimNextBuild(ctx, builderID, builderID)
	if err != nil {
		t.Fatalf("ClaimNextBuild(%s): %v", builderID, err)
	}
	if claimed.ID != expectedBuildID {
		t.Fatalf("expected build %q to be claimed by %s, got %q", expectedBuildID, builderID, claimed.ID)
	}
	return claimed
}

func assertNoClaim(t *testing.T, d *testDeliveryHarness, ctx context.Context, builderID string) {
	t.Helper()
	claimed, err := d.ClaimNextBuild(ctx, builderID, builderID)
	if err != nil {
		t.Fatalf("ClaimNextBuild(%s): %v", builderID, err)
	}
	if claimed.ID != "" {
		t.Fatalf("expected no claim for %s, got build %q", builderID, claimed.ID)
	}
}

func expireBuildLease(t *testing.T, store *persistence, ctx context.Context, buildID string) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE build_runs SET lease_expires_at = statement_timestamp() - INTERVAL '1 minute' WHERE id = $1`, buildID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func buildDigest(nibble string) string {
	return "registry.example.test/platform/web@sha256:" + strings.Repeat(nibble, 64)
}

func TestBuildLeaseWorkerDeathRequeuesWithBoundedRetry(t *testing.T) {
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-lease-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-lease-1")
	if err != nil {
		t.Fatal(err)
	}
	first := claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if first.AttemptCount != 1 || first.OwnerEpoch != 1 {
		t.Fatalf("first claim = attempt %d epoch %d, want attempt 1 epoch 1", first.AttemptCount, first.OwnerEpoch)
	}
	if err := testDelivery(store).HeartbeatBuild(ctx, "builder-1", build.ID, first.OwnerEpoch); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	expireBuildLease(t, store, ctx, build.ID)
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatalf("RecoverExpiredBuilds: %v", err)
	}
	requeued, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.State != deliverycore.BuildStateQueued || requeued.BuilderID != "" {
		t.Fatalf("requeued build = state %q owner %q, want queued with no owner", requeued.State, requeued.BuilderID)
	}

	second := claimBuildForTest(t, store, ctx, "builder-2", build.ID)
	if second.AttemptCount != 2 || second.OwnerEpoch != 2 {
		t.Fatalf("takeover claim = attempt %d epoch %d, want attempt 2 epoch 2", second.AttemptCount, second.OwnerEpoch)
	}
	if err := completeBuildForTest(ctx, store, "builder-2", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-lease-1", buildDigest("a"), ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	attempts, err := testDelivery(store).BuildAttempts(ctx, testUser("user-1"), service.ID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempt history has %d rows, want 2", len(attempts))
	}
	if attempts[0].Outcome != deliverycore.BuildAttemptWorkerLost || attempts[0].BuilderID != "builder-1" {
		t.Fatalf("attempt 1 = %+v, want worker_lost by builder-1", attempts[0])
	}
	if attempts[1].Outcome != deliverycore.BuildAttemptSucceeded || attempts[1].BuilderID != "builder-2" {
		t.Fatalf("attempt 2 = %+v, want succeeded by builder-2", attempts[1])
	}
}

func TestBuildLeaseRetryBudgetExhaustionIsTerminal(t *testing.T) {
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-retry-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-retry-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs SET attempt_limit = 1 WHERE id = $1`, build.ID); err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	expireBuildLease(t, store, ctx, build.ID)
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatalf("RecoverExpiredBuilds: %v", err)
	}
	failed, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != deliverycore.BuildStateFailed || !strings.Contains(failed.FailureReason, "retry budget exhausted") {
		t.Fatalf("exhausted build = state %q reason %q", failed.State, failed.FailureReason)
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID); got.State != deliverycore.DeploymentStateFailed {
		t.Fatalf("deployment after exhaustion = %q, want failed", got.State)
	}

	// Deterministic build failures are terminal immediately: no requeue, no retry.
	if err := seedReadySourceState(t, store, service, "commit-retry-2"); err != nil {
		t.Fatal(err)
	}
	deterministic, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-retry-2")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", deterministic.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", deterministic.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-retry-2", "", "dockerfile parse error"); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.reads.BuildByID(ctx, deterministic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != deliverycore.BuildStateFailed || after.AttemptCount != 1 {
		t.Fatalf("deterministic failure = state %q attempts %d, want failed with 1 attempt", after.State, after.AttemptCount)
	}
	attempts, err := testDelivery(store).BuildAttempts(ctx, testUser("user-1"), service.ID, deterministic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != deliverycore.BuildAttemptFailedTerminal {
		t.Fatalf("deterministic failure attempts = %+v, want one failed_terminal", attempts)
	}
}

func TestBuildLeaseSplitOwnershipFencing(t *testing.T) {
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-fence-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-fence-1")
	if err != nil {
		t.Fatal(err)
	}
	first := claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	expireBuildLease(t, store, ctx, build.ID)
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatal(err)
	}
	second := claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if second.OwnerEpoch != first.OwnerEpoch+1 {
		t.Fatalf("reclaim epoch = %d, want %d", second.OwnerEpoch, first.OwnerEpoch+1)
	}

	// The stale epoch fences heartbeat, log report, and completion.
	if err := testDelivery(store).HeartbeatBuild(ctx, "builder-1", build.ID, first.OwnerEpoch); !errors.Is(err, deliverycore.ErrBuildLeaseLost) {
		t.Fatalf("stale heartbeat = %v, want ErrBuildLeaseLost", err)
	}
	operations := NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil)
	staleLogs, err := operations.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{BuildId: build.ID, LeaseEpoch: first.OwnerEpoch, Lines: []*platformv1.BuildLogLine{{Line: "stale"}}},
	)
	if staleLogs != nil || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale log report = %v, %v, want PermissionDenied", staleLogs, err)
	}
	if err := completeBuildForTestWithEpoch(ctx, store, "builder-1", build.ID, first.OwnerEpoch, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-fence-1", buildDigest("b"), ""); !errors.Is(err, deliverycore.ErrBuildLeaseLost) {
		t.Fatalf("stale completion = %v, want ErrBuildLeaseLost", err)
	}
	fenced, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fenced.State != deliverycore.BuildStateRunning || fenced.OwnerEpoch != second.OwnerEpoch || fenced.ImageDigest != "" {
		t.Fatalf("stale completion mutated build: %+v", fenced)
	}

	// The current epoch still works end to end.
	if err := testDelivery(store).HeartbeatBuild(ctx, "builder-1", build.ID, second.OwnerEpoch); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if _, err := operations.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{BuildId: build.ID, LeaseEpoch: second.OwnerEpoch, Lines: []*platformv1.BuildLogLine{{Line: "current"}}},
	); err != nil {
		t.Fatalf("current log report: %v", err)
	}
	if err := completeBuildForTestWithEpoch(ctx, store, "builder-1", build.ID, second.OwnerEpoch, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-fence-1", buildDigest("b"), ""); err != nil {
		t.Fatalf("current completion: %v", err)
	}
}

func TestBuildCancelPreventsLateCompletionPublish(t *testing.T) {
	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-cancel-lease", "Cancel me", "Ada", source.BuildTransition{TrackedHead: true}); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-cancel-lease")
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	before, err := store.reads.ServiceByID(ctx, testUser(userID), service.ID)
	if err != nil {
		t.Fatal(err)
	}

	current := currentDeploymentForTest(t, store, ctx, service.ID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-lease-1", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	cancelling, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelling.State != deliverycore.BuildStateRunning || !cancelling.CancelRequestedAt.Valid {
		t.Fatalf("cancelled running build = state %q requested %v, want running with cancel_requested_at", cancelling.State, cancelling.CancelRequestedAt)
	}
	progress, err := store.reads.ServiceByID(ctx, testUser(userID), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.LatestBuild.GetCancelRequestedAt() == nil {
		t.Fatal("expected LatestBuild to expose cancel_requested_at while cancellation is in flight")
	}
	if err := testDelivery(store).HeartbeatBuild(ctx, "builder-1", build.ID, claimed.OwnerEpoch); !errors.Is(err, deliverycore.ErrBuildCancelled) {
		t.Fatalf("heartbeat after cancel = %v, want ErrBuildCancelled", err)
	}

	// The late success converges to cancelled: no image, no rollout.
	if err := completeBuildForTestWithEpoch(ctx, store, "builder-1", build.ID, claimed.OwnerEpoch, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-cancel-lease", buildDigest("c"), ""); err != nil {
		t.Fatalf("late completion: %v", err)
	}
	converged, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if converged.State != deliverycore.BuildStateCancelled || converged.ImageDigest != "" {
		t.Fatalf("late completion = state %q image %q, want cancelled with no image", converged.State, converged.ImageDigest)
	}
	after, err := store.reads.ServiceByID(ctx, testUser(userID), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ResolvedImage != before.ResolvedImage || after.RolloutGeneration != before.RolloutGeneration {
		t.Fatalf("late completion published image %q rollout %d, want image %q rollout %d",
			after.ResolvedImage, after.RolloutGeneration, before.ResolvedImage, before.RolloutGeneration)
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID); got.State != deliverycore.DeploymentStateCancelled {
		t.Fatalf("deployment after late completion = %q, want cancelled", got.State)
	}
	attempts, err := testDelivery(store).BuildAttempts(ctx, testUser(userID), service.ID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != deliverycore.BuildAttemptCancelled {
		t.Fatalf("cancelled attempts = %+v, want one cancelled", attempts)
	}

	// The released cap is usable immediately.
	if err := seedReadySourceState(t, store, service, "commit-cancel-lease-2"); err != nil {
		t.Fatal(err)
	}
	next, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-cancel-lease-2")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", next.ID)
}

func TestCancelledBuildPastDeadlineRemainsCancelled(t *testing.T) {
	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "commit-cancel-timeout"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-cancel-timeout")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	current := currentDeploymentForTest(t, store, ctx, service.ID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-timeout", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs SET deadline_at = statement_timestamp() - INTERVAL '1 minute' WHERE id = $1`, build.ID); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).RecoverExpiredBuilds(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != deliverycore.BuildStateCancelled {
		t.Fatalf("timed-out cancellation state = %q", completed.State)
	}
	attempts, err := testDelivery(store).BuildAttempts(ctx, testUser(userID), service.ID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != deliverycore.BuildAttemptCancelled {
		t.Fatalf("timed-out cancellation attempts = %+v", attempts)
	}
}

func TestBuildSchedulerGlobalAndProjectCaps(t *testing.T) {
	store, ctx, _, projectID, serviceA := setupSourceServiceForDeployment(t)
	serviceB := createSecondRepoService(t, store, ctx, projectID)
	d := schedulerTestDelivery(store, deliverycore.BuildSchedulerConfig{
		MaxConcurrentGlobal: 10, MaxConcurrentPerProject: 1,
	}.WithDefaults())

	if err := seedReadySourceState(t, store, serviceA, "commit-cap-a1"); err != nil {
		t.Fatal(err)
	}
	buildA1, err := enqueueBuildForTest(ctx, store, "user-1", serviceA.ID, "commit-cap-a1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, serviceB, "commit-cap-b1"); err != nil {
		t.Fatal(err)
	}
	buildB1, err := enqueueBuildForTest(ctx, store, "user-1", serviceB.ID, "commit-cap-b1")
	if err != nil {
		t.Fatal(err)
	}

	claimBuildWith(t, d, ctx, "builder-1", buildA1.ID)
	assertNoClaim(t, d, ctx, "builder-2")
	if err := completeBuildForTest(ctx, store, "builder-1", buildA1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-cap-a1", buildDigest("d"), ""); err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-2", buildB1.ID)

	d.SetBuildSchedulerConfigForTest(deliverycore.BuildSchedulerConfig{
		MaxConcurrentGlobal: 1, MaxConcurrentPerProject: 10,
	}.WithDefaults())
	if err := seedReadySourceState(t, store, serviceA, "commit-cap-a2"); err != nil {
		t.Fatal(err)
	}
	buildA2, err := enqueueBuildForTest(ctx, store, "user-1", serviceA.ID, "commit-cap-a2")
	if err != nil {
		t.Fatal(err)
	}
	assertNoClaim(t, d, ctx, "builder-3")
	if err := completeBuildForTest(ctx, store, "builder-2", buildB1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-cap-b1", buildDigest("e"), ""); err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-3", buildA2.ID)
}

func TestBuildSchedulerReleasesCapOnRunningTerminalPaths(t *testing.T) {
	store, ctx, userID, projectID, serviceA := setupSourceServiceForDeployment(t)
	serviceB := createSecondRepoService(t, store, ctx, projectID)
	d := schedulerTestDelivery(store, deliverycore.BuildSchedulerConfig{
		MaxConcurrentGlobal: 1, MaxConcurrentPerProject: 10,
	}.WithDefaults())

	paths := []struct {
		name     string
		terminal func(t *testing.T, build deliverycore.BuildRunRecord, commit string)
		assert   func(t *testing.T, build deliverycore.BuildRunRecord)
	}{
		{
			name: "success",
			terminal: func(t *testing.T, build deliverycore.BuildRunRecord, commit string) {
				t.Helper()
				if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, commit, buildDigest("f"), ""); err != nil {
					t.Fatalf("complete success: %v", err)
				}
			},
			assert: func(t *testing.T, build deliverycore.BuildRunRecord) {
				t.Helper()
				if build.State != deliverycore.BuildStateSucceeded {
					t.Fatalf("state = %q, want succeeded", build.State)
				}
			},
		},
		{
			name: "deterministic-failure",
			terminal: func(t *testing.T, build deliverycore.BuildRunRecord, commit string) {
				t.Helper()
				if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, commit, "", "railpack exit 1"); err != nil {
					t.Fatalf("complete failed: %v", err)
				}
			},
			assert: func(t *testing.T, build deliverycore.BuildRunRecord) {
				t.Helper()
				if build.State != deliverycore.BuildStateFailed {
					t.Fatalf("state = %q, want failed", build.State)
				}
			},
		},
		{
			name: "running-cancel",
			terminal: func(t *testing.T, build deliverycore.BuildRunRecord, commit string) {
				t.Helper()
				current := currentDeploymentForTest(t, store, ctx, serviceA.ID)
				if _, _, err := applyDeploymentActionForTest(ctx, store, userID, serviceA.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-cap-"+commit, ""); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, commit, buildDigest("f"), ""); err != nil {
					t.Fatalf("late completion after cancel: %v", err)
				}
			},
			assert: func(t *testing.T, build deliverycore.BuildRunRecord) {
				t.Helper()
				if build.State != deliverycore.BuildStateCancelled || build.ImageDigest != "" {
					t.Fatalf("state = %q image %q, want cancelled with no image", build.State, build.ImageDigest)
				}
			},
		},
		{
			name: "timeout",
			terminal: func(t *testing.T, build deliverycore.BuildRunRecord, commit string) {
				t.Helper()
				if _, err := store.db.ExecContext(ctx,
					`UPDATE build_runs SET deadline_at = statement_timestamp() - INTERVAL '1 minute' WHERE id = $1`, build.ID); err != nil {
					t.Fatalf("expire deadline: %v", err)
				}
				if err := d.RecoverExpiredBuilds(ctx); err != nil {
					t.Fatalf("RecoverExpiredBuilds: %v", err)
				}
			},
			assert: func(t *testing.T, build deliverycore.BuildRunRecord) {
				t.Helper()
				if build.State != deliverycore.BuildStateFailed || !strings.Contains(build.FailureReason, "timeout") {
					t.Fatalf("state = %q reason %q, want failed on timeout", build.State, build.FailureReason)
				}
				attempts, err := d.BuildAttempts(ctx, testUser(userID), serviceA.ID, build.ID)
				if err != nil {
					t.Fatalf("BuildAttempts: %v", err)
				}
				if len(attempts) != 1 || attempts[0].Outcome != deliverycore.BuildAttemptTimedOut {
					t.Fatalf("timeout attempts = %+v, want one timed_out", attempts)
				}
				if got := currentDeploymentForTest(t, store, ctx, serviceA.ID); got.State != deliverycore.DeploymentStateFailed {
					t.Fatalf("deployment after timeout = %q, want failed", got.State)
				}
			},
		},
		{
			name: "lease-supersede",
			terminal: func(t *testing.T, build deliverycore.BuildRunRecord, commit string) {
				t.Helper()
				newerCommit := commit + "-newer"
				if err := seedReadySourceState(t, store, serviceA, newerCommit); err != nil {
					t.Fatalf("seed newer: %v", err)
				}
				newer, err := enqueueBuildForTest(ctx, store, userID, serviceA.ID, newerCommit)
				if err != nil {
					t.Fatalf("enqueue newer: %v", err)
				}
				expireBuildLease(t, store, ctx, build.ID)
				if err := d.RecoverExpiredBuilds(ctx); err != nil {
					t.Fatalf("RecoverExpiredBuilds: %v", err)
				}
				// The newer build is queued behind the other service's build;
				// cancel it so later subtests start from a clean queue.
				current := currentDeploymentForTest(t, store, ctx, serviceA.ID)
				if current.BuildID != newer.ID {
					t.Fatalf("current deployment build = %q, want newer build %q", current.BuildID, newer.ID)
				}
				if _, _, err := applyDeploymentActionForTest(ctx, store, userID, serviceA.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-newer-"+commit, ""); err != nil {
					t.Fatalf("cancel newer: %v", err)
				}
			},
			assert: func(t *testing.T, build deliverycore.BuildRunRecord) {
				t.Helper()
				if build.State != deliverycore.BuildStateSuperseded {
					t.Fatalf("state = %q, want superseded", build.State)
				}
			},
		},
	}

	for i, path := range paths {
		commitA := fmt.Sprintf("commit-term-a-%d", i)
		commitB := fmt.Sprintf("commit-term-b-%d", i)
		if err := seedReadySourceState(t, store, serviceA, commitA); err != nil {
			t.Fatal(err)
		}
		buildA, err := enqueueBuildForTest(ctx, store, userID, serviceA.ID, commitA)
		if err != nil {
			t.Fatal(err)
		}
		claimBuildWith(t, d, ctx, "builder-1", buildA.ID)
		if err := seedReadySourceState(t, store, serviceB, commitB); err != nil {
			t.Fatal(err)
		}
		buildB, err := enqueueBuildForTest(ctx, store, userID, serviceB.ID, commitB)
		if err != nil {
			t.Fatal(err)
		}
		assertNoClaim(t, d, ctx, "builder-2")

		path.terminal(t, buildA, commitA)
		terminal, err := store.reads.BuildByID(ctx, buildA.ID)
		if err != nil {
			t.Fatal(err)
		}
		path.assert(t, terminal)

		// Every terminal path releases the global cap for the waiting build.
		claimBuildWith(t, d, ctx, "builder-2", buildB.ID)
		if err := completeBuildForTest(ctx, store, "builder-2", buildB.ID, platformv1.BuildState_BUILD_STATE_FAILED, commitB, "", "cleanup"); err != nil {
			t.Fatalf("%s: cleanup completion: %v", path.name, err)
		}
	}
}

func TestBuildSchedulerQueuedTerminalPaths(t *testing.T) {
	store, ctx, userID, _, service := setupSourceServiceForDeployment(t)
	d := schedulerTestDelivery(store, deliverycore.DefaultBuildSchedulerConfig())

	// Cancelling a queued build finishes it immediately: no lease, no wait
	// for a builder, and the queue keeps moving.
	if err := seedReadySourceState(t, store, service, "commit-queued-cancel"); err != nil {
		t.Fatal(err)
	}
	queued, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-queued-cancel")
	if err != nil {
		t.Fatal(err)
	}
	current := currentDeploymentForTest(t, store, ctx, service.ID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, current.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, "cancel-queued-1", ""); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	cancelled, err := store.reads.BuildByID(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != deliverycore.BuildStateCancelled || !cancelled.FinishedAt.Valid {
		t.Fatalf("queued cancel = state %q finished %v, want cancelled with finished_at", cancelled.State, cancelled.FinishedAt)
	}

	// A build that waits longer than the maximum queue age fails instead of
	// running stale.
	if err := seedReadySourceState(t, store, service, "commit-queue-age"); err != nil {
		t.Fatal(err)
	}
	stale, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-queue-age")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE build_runs SET queued_at = statement_timestamp() - INTERVAL '3 hours' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverExpiredBuilds(ctx); err != nil {
		t.Fatal(err)
	}
	expired, err := store.reads.BuildByID(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != deliverycore.BuildStateFailed || !strings.Contains(expired.FailureReason, "maximum queue age") {
		t.Fatalf("queue expiry = state %q reason %q", expired.State, expired.FailureReason)
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID); got.State != deliverycore.DeploymentStateFailed {
		t.Fatalf("deployment after queue expiry = %q, want failed", got.State)
	}

	if err := seedReadySourceState(t, store, service, "commit-queue-next"); err != nil {
		t.Fatal(err)
	}
	next, err := enqueueBuildForTest(ctx, store, userID, service.ID, "commit-queue-next")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-1", next.ID)
}

func TestBuildSchedulerDrainAndPause(t *testing.T) {
	store, ctx, userID, projectID, serviceA := setupSourceServiceForDeployment(t)
	serviceB := createSecondRepoService(t, store, ctx, projectID)
	d := schedulerTestDelivery(store, deliverycore.DefaultBuildSchedulerConfig())

	if _, err := d.ListBuilders(ctx, testUser(userID)); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("non-operator ListBuilders = %v, want ErrDenied", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO platform_operators(user_id, created_at) VALUES ('user-1', statement_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetBuilderDrain(ctx, testUser(userID), "builder-unknown", true); err == nil {
		t.Fatal("expected draining an unknown builder to fail")
	}

	if err := seedReadySourceState(t, store, serviceA, "commit-drain-a1"); err != nil {
		t.Fatal(err)
	}
	buildA1, err := enqueueBuildForTest(ctx, store, userID, serviceA.ID, "commit-drain-a1")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-1", buildA1.ID)

	builders, err := d.ListBuilders(ctx, testUser(userID))
	if err != nil {
		t.Fatal(err)
	}
	if len(builders) != 1 || builders[0].ID != "builder-1" || builders[0].CurrentBuildID != buildA1.ID {
		t.Fatalf("builders = %+v, want builder-1 on build %q", builders, buildA1.ID)
	}

	// A drained builder finishes its running build but takes no new work.
	drained, err := d.SetBuilderDrain(ctx, testUser(userID), "builder-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !drained.Drained {
		t.Fatal("expected builder-1 to be drained")
	}
	if err := seedReadySourceState(t, store, serviceB, "commit-drain-b1"); err != nil {
		t.Fatal(err)
	}
	buildB1, err := enqueueBuildForTest(ctx, store, userID, serviceB.ID, "commit-drain-b1")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-1", buildA1.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", buildA1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-drain-a1", buildDigest("d"), ""); err != nil {
		t.Fatal(err)
	}
	assertNoClaim(t, d, ctx, "builder-1")
	claimBuildWith(t, d, ctx, "builder-2", buildB1.ID)
	if _, err := d.SetBuilderDrain(ctx, testUser(userID), "builder-1", false); err != nil {
		t.Fatal(err)
	}

	// A paused scheduler likewise lets running work finish while new claims wait.
	if err := seedReadySourceState(t, store, serviceA, "commit-drain-a2"); err != nil {
		t.Fatal(err)
	}
	buildA2, err := enqueueBuildForTest(ctx, store, userID, serviceA.ID, "commit-drain-a2")
	if err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-1", buildA2.ID)
	paused, err := d.SetBuildSchedulerPaused(ctx, testUser(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused {
		t.Fatal("expected scheduler to be paused")
	}
	if err := seedReadySourceState(t, store, serviceB, "commit-drain-b2"); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueueBuildForTest(ctx, store, userID, serviceB.ID, "commit-drain-b2"); err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-1", buildA2.ID)
	assertNoClaim(t, d, ctx, "builder-3")
	state, err := d.BuildSchedulerState(ctx, testUser(userID))
	if err != nil {
		t.Fatal(err)
	}
	if state.RunningBuilds != 2 || state.QueuedBuilds != 1 {
		t.Fatalf("scheduler state = running %d queued %d, want running 2 queued 1", state.RunningBuilds, state.QueuedBuilds)
	}
	if err := completeBuildForTest(ctx, store, "builder-1", buildA2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-drain-a2", buildDigest("d"), ""); err != nil {
		t.Fatal(err)
	}
	assertNoClaim(t, d, ctx, "builder-3")
	if _, err := d.SetBuildSchedulerPaused(ctx, testUser(userID), false); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.ClaimNextBuild(ctx, "builder-3", "builder-3")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID == "" {
		t.Fatal("expected builder-3 to claim the queued build after unpause")
	}
}
