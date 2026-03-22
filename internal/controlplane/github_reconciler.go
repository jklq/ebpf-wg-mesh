package controlplane

import (
	"context"
	"log/slog"
	"time"
)

type GitHubReconciler struct {
	store             *Store
	coordinator       *GitHubCoordinator
	buildStaleAfter   time.Duration
	webhookStaleAfter time.Duration
	workStaleAfter    time.Duration
}

func NewGitHubReconciler(store *Store, coordinator *GitHubCoordinator, buildStaleAfter, webhookStaleAfter, workStaleAfter time.Duration) *GitHubReconciler {
	if store == nil || coordinator == nil || !coordinator.Enabled() {
		return nil
	}
	return &GitHubReconciler{
		store:             store,
		coordinator:       coordinator,
		buildStaleAfter:   buildStaleAfter,
		webhookStaleAfter: webhookStaleAfter,
		workStaleAfter:    workStaleAfter,
	}
}

func (r *GitHubReconciler) Bootstrap(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if err := r.store.recoverExpiredBuilds(ctx, r.buildStaleAfter); err != nil {
		return err
	}
	if err := r.store.recoverGitHubWebhookDeliveries(ctx, r.webhookStaleAfter); err != nil {
		return err
	}
	if err := r.store.recoverSourceWorkItems(ctx, r.workStaleAfter); err != nil {
		return err
	}
	installationIDs, err := r.store.listActiveGitHubInstallationIDs(ctx)
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		processed, err := r.processNext(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *GitHubReconciler) processNext(ctx context.Context) (bool, error) {
	rec, err := r.store.claimNextSourceWorkItem(ctx, "github-reconciler", r.workStaleAfter)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		return false, nil
	}
	if err := r.coordinator.processWorkItem(ctx, rec); err != nil {
		if releaseErr := r.store.releaseSourceWorkItem(ctx, rec.ID, err, r.coordinator.retryAfter); releaseErr != nil {
			return false, releaseErr
		}
		if err != errGitHubWorkDeferred {
			slog.Warn("github work item failed", "kind", rec.Kind, "service_id", rec.ServiceID, "provider_scope_external_id", rec.ProviderScopeExternalID, "error", err)
		}
		return true, nil
	}
	if err := r.store.completeSourceWorkItem(ctx, rec.ID); err != nil {
		return false, err
	}
	return true, nil
}
