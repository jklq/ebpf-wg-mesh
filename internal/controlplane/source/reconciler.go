package source

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

type GitHubReconciler struct {
	store             Store
	coordinator       *GitHubCoordinator
	buildStaleAfter   time.Duration
	webhookStaleAfter time.Duration
	workStaleAfter    time.Duration
	workerID          string
}

func NewGitHubReconciler(store Store, coordinator *GitHubCoordinator, buildStaleAfter, webhookStaleAfter, workStaleAfter time.Duration) *GitHubReconciler {
	if store == nil || coordinator == nil || !coordinator.Enabled() {
		return nil
	}
	return &GitHubReconciler{
		store:             store,
		coordinator:       coordinator,
		buildStaleAfter:   buildStaleAfter,
		webhookStaleAfter: webhookStaleAfter,
		workStaleAfter:    workStaleAfter,
		workerID:          "github-reconciler-" + uuid.NewString(),
	}
}

func (r *GitHubReconciler) Bootstrap(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if err := r.coordinator.delivery.RecoverExpiredBuilds(ctx, r.buildStaleAfter); err != nil {
		return err
	}
	if err := r.store.RecoverGitHubWebhookDeliveries(ctx, r.webhookStaleAfter); err != nil {
		return err
	}
	if err := r.coordinator.RecoverWorkItems(ctx, r.workStaleAfter); err != nil {
		return err
	}
	installationIDs, err := r.store.ListActiveGitHubInstallationIDs(ctx)
	if err != nil {
		return err
	}
	for _, installationID := range installationIDs {
		if err := r.coordinator.RequestInstallationRefresh(ctx, installationID); err != nil {
			return err
		}
	}
	return nil
}

func (r *GitHubReconciler) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	const maxRetryDelay = 30 * time.Second
	retryDelay := 250 * time.Millisecond
	for {
		processed, err := r.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("github reconciler pass failed", "error", err, "retry_after", retryDelay)
			if !waitContext(ctx, jitter(retryDelay)) {
				return nil
			}
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
			continue
		}
		retryDelay = 250 * time.Millisecond
		if processed {
			continue
		}
		if !waitContext(ctx, jitter(time.Second)) {
			return nil
		}
	}
}

func (r *GitHubReconciler) ProcessNext(ctx context.Context) (bool, error) {
	rec, err := r.coordinator.ClaimNextWorkItem(ctx, r.workerID, r.workStaleAfter)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		return false, nil
	}
	slog.InfoContext(ctx, "github work item claimed", "kind", rec.Kind, "service_id", rec.ServiceID, "provider_scope_external_id", rec.ProviderScopeExternalID, "provider_repository_external_id", rec.ProviderRepositoryExternalID, "tracked_ref", rec.TrackedRef, "commit_sha", rec.CommitSHA)
	if err := r.coordinator.processWorkItem(ctx, rec); err != nil {
		if releaseErr := r.coordinator.ReleaseWorkItem(ctx, rec.ID, r.workerID, err, r.coordinator.retryAfter); releaseErr != nil {
			return false, releaseErr
		}
		if err != errGitHubWorkDeferred {
			slog.Warn("github work item failed", "kind", rec.Kind, "service_id", rec.ServiceID, "provider_scope_external_id", rec.ProviderScopeExternalID, "error", err)
		}
		return true, nil
	}
	if err := r.coordinator.CompleteWorkItem(ctx, rec.ID, r.workerID); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "github work item completed", "kind", rec.Kind, "service_id", rec.ServiceID, "provider_scope_external_id", rec.ProviderScopeExternalID, "provider_repository_external_id", rec.ProviderRepositoryExternalID, "tracked_ref", rec.TrackedRef, "commit_sha", rec.CommitSHA)
	return true, nil
}
