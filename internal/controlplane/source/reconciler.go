package source

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

type GitHubReconciler struct {
	store             Store
	coordinator       *GitHubCoordinator
	buildStaleAfter   time.Duration
	webhookStaleAfter time.Duration
	workerID          string
}

func NewGitHubReconciler(store Store, coordinator *GitHubCoordinator, buildStaleAfter, webhookStaleAfter time.Duration) *GitHubReconciler {
	if store == nil || coordinator == nil || !coordinator.Enabled() {
		return nil
	}
	return &GitHubReconciler{
		store:             store,
		coordinator:       coordinator,
		buildStaleAfter:   buildStaleAfter,
		webhookStaleAfter: webhookStaleAfter,
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
	// Source work needs no recovery scan: records are leased with expiry and
	// the next claim transparently takes over whatever a dead worker left
	// behind.
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
	idleDelay := time.Second
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
			idleDelay = time.Second
			continue
		}
		if !r.coordinator.waitForWork(ctx, jitter(idleDelay)) {
			return nil
		}
		idleDelay *= 2
		if idleDelay > maxRetryDelay {
			idleDelay = maxRetryDelay
		}
	}
}

func (r *GitHubReconciler) ProcessNext(ctx context.Context) (bool, error) {
	rec, err := r.coordinator.ClaimNextWorkItem(ctx, r.workerID)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		return false, nil
	}
	payload, err := DecodeWorkPayload(rec.Payload)
	if err != nil {
		// A corrupt payload can never succeed: fail it without retry so it
		// lands in the failed state for inspection instead of burning the
		// attempt budget.
		if failErr := r.coordinator.FailWorkItem(ctx, rec, err, false); failErr != nil {
			return false, failErr
		}
		slog.Warn("github work item had a corrupt payload", "kind", rec.Kind, "dedup_key", rec.DedupKey, "error", err)
		return true, nil
	}
	slog.InfoContext(ctx, "github work item claimed", "kind", rec.Kind, "service_id", payload.ServiceID, "provider_scope_external_id", payload.ProviderScopeExternalID, "provider_repository_external_id", payload.ProviderRepositoryExternalID, "tracked_ref", payload.TrackedRef, "commit_sha", payload.CommitSHA)
	if err := r.coordinator.processWorkItem(ctx, rec, payload); err != nil {
		retryable := !errors.Is(err, errUnknownWorkKind)
		if failErr := r.coordinator.FailWorkItem(ctx, rec, err, retryable); failErr != nil {
			return false, failErr
		}
		if err != errGitHubWorkDeferred {
			slog.Warn("github work item failed", "kind", rec.Kind, "service_id", payload.ServiceID, "provider_scope_external_id", payload.ProviderScopeExternalID, "retryable", retryable, "error", err)
		}
		return true, nil
	}
	if err := r.coordinator.CompleteWorkItem(ctx, rec); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "github work item completed", "kind", rec.Kind, "service_id", payload.ServiceID, "provider_scope_external_id", payload.ProviderScopeExternalID, "provider_repository_external_id", payload.ProviderRepositoryExternalID, "tracked_ref", payload.TrackedRef, "commit_sha", payload.CommitSHA)
	return true, nil
}
