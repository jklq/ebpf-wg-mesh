package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/durablework"
)

var errGitHubWorkDeferred = errors.New("github work deferred")

var errUnknownWorkKind = errors.New("unknown source work kind")

// errSourcePushPredecessorPending defers a revision_observed item whose
// push transition names a predecessor that is not observed yet: the
// successor webhook arrived before its predecessor's. The work loop
// requeues it (bounded retry with backoff) until the predecessor advances
// the binding head and the successor can prove currency.
var errSourcePushPredecessorPending = errors.New("source push predecessor not observed yet")

type GitHubCoordinator struct {
	store      Store
	work       *durablework.Store
	delivery   Delivery
	catalog    *GitHubCatalog
	client     *GitHubClient
	staleAfter time.Duration
}

type sourceWorkWakeup interface {
	SourceWorkReady() <-chan struct{}
}

func NewGitHubCoordinator(store Store, work *durablework.Store, delivery Delivery, catalog *GitHubCatalog, client *GitHubClient, staleAfter time.Duration) *GitHubCoordinator {
	if store == nil || work == nil || delivery == nil || catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	return &GitHubCoordinator{
		store:      store,
		work:       work,
		delivery:   delivery,
		catalog:    catalog,
		client:     client,
		staleAfter: staleAfter,
	}
}

func (c *GitHubCoordinator) waitForWork(ctx context.Context, delay time.Duration) bool {
	if wakeup, ok := c.store.(sourceWorkWakeup); ok {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-wakeup.SourceWorkReady():
			return true
		}
	}
	return waitContext(ctx, delay)
}

func (c *GitHubCoordinator) Enabled() bool {
	return c != nil && c.store != nil && c.catalog != nil && c.client != nil
}

func (c *GitHubCoordinator) ClaimNextWorkItem(ctx context.Context, ownerID string) (durablework.Record, error) {
	return c.work.Claim(ctx, ownerID, WorkLeaseTTL, SourceWorkKinds...)
}

func (c *GitHubCoordinator) CompleteWorkItem(ctx context.Context, rec durablework.Record) error {
	return c.work.Complete(ctx, rec)
}

// FailWorkItem dispositions a claimed record after a failed attempt.
// Retryable failures requeue with jittered backoff and dead-letter when the
// attempt limit is reached; non-retryable failures (poison payloads,
// unknown kinds) move the record to failed at once.
func (c *GitHubCoordinator) FailWorkItem(ctx context.Context, rec durablework.Record, processErr error, retryable bool) error {
	return c.work.Fail(ctx, rec, processErr, durablework.FailOptions{Retryable: retryable})
}

func (c *GitHubCoordinator) RequestInstallationRefresh(ctx context.Context, installationID int64) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	if installationID <= 0 {
		return errors.New("installation id is required")
	}
	inserted, err := c.work.Enqueue(ctx, ProviderAccessChangedParams(installationID))
	if err != nil {
		return err
	}
	if inserted {
		slog.InfoContext(ctx, "github installation refresh queued", "installation_id", installationID)
	} else {
		slog.InfoContext(ctx, "github installation refresh deduplicated", "installation_id", installationID)
	}
	return err
}

func (c *GitHubCoordinator) ObserveRepositoryRevision(ctx context.Context, repositoryExternalID, trackedRef, commitSHA, previousCommitSHA, commitMessage, commitAuthor string) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	repositoryExternalID = strings.TrimSpace(repositoryExternalID)
	trackedRef = strings.TrimSpace(trackedRef)
	commitSHA = strings.TrimSpace(commitSHA)
	if repositoryExternalID == "" || trackedRef == "" || commitSHA == "" {
		return errors.New("repository id, tracked ref, and commit sha are required")
	}
	inserted, err := c.work.Enqueue(ctx, RevisionObservedParams(repositoryExternalID, trackedRef, commitSHA, previousCommitSHA, commitMessage, commitAuthor))
	if err != nil {
		return err
	}
	if inserted {
		slog.InfoContext(ctx, "github revision observed", "repository_external_id", repositoryExternalID, "tracked_ref", trackedRef, "commit_sha", commitSHA)
	} else {
		slog.InfoContext(ctx, "github revision already observed", "repository_external_id", repositoryExternalID, "tracked_ref", trackedRef, "commit_sha", commitSHA)
	}
	return nil
}

func (c *GitHubCoordinator) processWorkItem(ctx context.Context, rec durablework.Record, payload WorkPayload) error {
	switch rec.Kind {
	case SourceWorkKindProviderAccessChanged:
		return c.handleProviderAccessChanged(ctx, payload.ProviderScopeExternalID)
	case SourceWorkKindSourceSpecChanged:
		return c.syncServiceSource(ctx, payload.ServiceID, payload.SpecRevision)
	case SourceWorkKindRevisionObserved:
		return c.handleRevisionObserved(ctx, payload)
	default:
		return fmt.Errorf("%w: %q", errUnknownWorkKind, rec.Kind)
	}
}

func (c *GitHubCoordinator) handleProviderAccessChanged(ctx context.Context, providerScopeExternalID string) error {
	installationID := providerScopeExternalIDToInstallationID(providerScopeExternalID)
	if installationID <= 0 {
		return errors.New("provider scope external id is required")
	}
	if err := c.refreshInstallation(ctx, installationID); err != nil {
		return err
	}
	bindings, err := c.store.SourceBindingsForProviderScope(ctx, "github", providerScopeExternalID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if _, err := c.work.Enqueue(ctx, SourceResyncParams(binding.ServiceID)); err != nil {
			return err
		}
	}
	return nil
}

func (c *GitHubCoordinator) refreshInstallation(ctx context.Context, installationID int64) error {
	rec, err := c.store.GitHubInstallationByID(ctx, installationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		rec = GitHubInstallationRecord{
			InstallationID: installationID,
			Active:         true,
		}
	}
	return c.catalog.RefreshInstallation(ctx, githubInstallationView{
		InstallationID: rec.InstallationID,
		AccountLogin:   rec.AccountLogin,
		AccountType:    rec.AccountType,
		TargetType:     rec.TargetType,
	})
}

func (c *GitHubCoordinator) syncServiceSource(ctx context.Context, serviceID string, specRevision int64) error {
	service, err := c.store.ServiceSnapshot(ctx, serviceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if service.Deleted {
		return nil
	}
	if specRevision > 0 && service.SpecRevision != specRevision {
		return nil
	}
	source := DesiredSourceSpec(service.Spec)
	if source == nil {
		return nil
	}
	if strings.TrimSpace(strings.ToLower(source.GetProvider())) != "github" {
		return nil
	}
	owner, repo, err := SplitGitHubRepositorySelector(source.GetRepositorySelector())
	if err != nil {
		return err
	}
	installationID, err := c.store.ProjectGitHubRepositoryInstallation(ctx, service.ProjectID, owner, repo)
	if err != nil {
		return fmt.Errorf("repository is not linked to project: %w", err)
	}

	preView, err := c.catalog.RepositoryView(ctx, owner, repo, installationID)
	if err != nil {
		return err
	}
	if preView.InstallationID > 0 && gitHubViewNeedsRefresh(preView, preView.InstallationID, c.staleAfter, time.Now().UTC()) {
		if err := c.RequestInstallationRefresh(ctx, preView.InstallationID); err != nil {
			return err
		}
		return errGitHubWorkDeferred
	}
	if preView.InstallationID == 0 && preView.SnapshotUpdatedAt.IsZero() {
		if err := c.refreshRepositorySnapshot(ctx, owner, repo); err != nil {
			return err
		}
	}
	view, err := c.catalog.RepositoryView(ctx, owner, repo, installationID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	trackedRef := sourceTrackedRef(source, view.DefaultBranch)
	binding, err := c.store.UpsertSourceBinding(ctx, SourceBindingRecord{
		ServiceID:                    service.ID,
		ProjectID:                    service.ProjectID,
		Provider:                     "github",
		RepositorySelector:           sourceRepositorySelector(source),
		TrackedRef:                   trackedRef,
		ProviderRepositoryExternalID: fmt.Sprintf("%d", view.RepositoryID),
		ProviderScopeExternalID:      ScopeExternalID(view.InstallationID),
		AccessState:                  sourceAccessStateFromGitHubView(view),
		BuildRecipe:                  buildRecipeFromSourceSpec(source),
		ResolvedAt:                   now,
		FreshUntil:                   now.Add(c.staleAfter),
	})
	if err != nil {
		return err
	}
	if binding.AccessState != SourceAccessStateAvailable {
		return nil
	}
	// The fetch below proves currency only over the head observed before it
	// began (see BuildTransition): a push that lands while the fetch is in
	// flight makes this observation stale, and a delayed sync must never
	// move the proven head backward.
	fetchedFrom, err := c.store.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil {
		return err
	}
	commitSHA, err := c.client.GetBranchHead(ctx, view.Owner, view.Repo, trackedRef, providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID))
	if err != nil {
		return err
	}
	metadata, err := c.client.GetCommitMetadata(ctx, view.Owner, view.Repo, commitSHA, providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID))
	if err != nil {
		return err
	}
	// Unanchored syncs carry no spec revision: they are automatic refreshes
	// triggered by push or provider events, not explicit spec writes or
	// releases, so they honor the environment auto-deploy switch. The
	// commit is the freshly fetched tracked head, so the request is
	// authoritative regardless of observed order.
	return c.observeBoundRevision(ctx, binding, commitSHA, metadata.Message, metadata.Author, specRevision == 0, BuildTransition{TrackedHead: true, FetchedFromHead: fetchedFrom})
}

func (c *GitHubCoordinator) handleRevisionObserved(ctx context.Context, payload WorkPayload) error {
	bindings, err := c.store.SourceBindingsForGitHubRepositoryAndRef(ctx, payload.ProviderRepositoryExternalID, payload.TrackedRef)
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		slog.InfoContext(ctx, "github revision had no bound services", "repository_external_id", payload.ProviderRepositoryExternalID, "tracked_ref", payload.TrackedRef, "commit_sha", payload.CommitSHA)
		return nil
	}
	for _, binding := range bindings {
		transition := BuildTransition{PreviousCommit: payload.PreviousCommitSHA}
		stalePush := false
		if NoPushPredecessor(payload.PreviousCommitSHA) {
			// A created or recreated ref: the payload's all-zero "before"
			// names a predecessor that can never be observed — the
			// binding's stored tip predates the deletion — so chaining to
			// it would pend this push as an early successor forever. Prove
			// currency the way syncs do instead: fetch the tracked head.
			// The pushed commit must still be it; anything newer made this
			// delivery stale.
			fetchedFrom, err := c.store.SourceBindingHeadCommit(ctx, binding.ID)
			if err != nil {
				return err
			}
			owner, repo, err := SplitGitHubRepositorySelector(binding.RepositorySelector)
			if err != nil {
				return err
			}
			head, err := c.client.GetBranchHead(ctx, owner, repo, binding.TrackedRef, providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID))
			if err != nil {
				return err
			}
			if head == payload.CommitSHA {
				transition = BuildTransition{TrackedHead: true, FetchedFromHead: fetchedFrom}
			} else {
				// The ref moved past this delivery before it was applied:
				// record it as history only — never as a head, even when the
				// binding has none yet — and let the push that moved the head
				// carry the work.
				slog.InfoContext(ctx, "github recreated-ref push superseded by newer tracked head; recording only", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", payload.CommitSHA, "tracked_head", head)
				stalePush = true
				transition = BuildTransition{History: true}
			}
		}
		if time.Now().UTC().After(binding.FreshUntil) {
			slog.InfoContext(ctx, "github source binding stale; requesting refresh", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", payload.CommitSHA)
			if binding.AccessState == SourceAccessStateAvailable {
				if _, err := c.recordBoundRevision(ctx, binding, payload.CommitSHA, payload.CommitMessage, payload.CommitAuthor, transition); err != nil {
					return err
				}
			}
			if _, err := c.work.Enqueue(ctx, SourceSpecChangedParams(binding.ServiceID, 0, true)); err != nil {
				return err
			}
			continue
		}
		if binding.AccessState != SourceAccessStateAvailable {
			slog.InfoContext(ctx, "github source binding unavailable", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", payload.CommitSHA, "access_state", binding.AccessState)
			continue
		}
		if stalePush {
			if _, err := c.recordBoundRevision(ctx, binding, payload.CommitSHA, payload.CommitMessage, payload.CommitAuthor, transition); err != nil {
				return err
			}
			continue
		}
		slog.InfoContext(ctx, "github revision matched bound service", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", payload.CommitSHA)
		if err := c.observeBoundRevision(ctx, binding, payload.CommitSHA, payload.CommitMessage, payload.CommitAuthor, true, transition); err != nil {
			return err
		}
	}
	return nil
}

// observeBoundRevision records the observed commit for the bound service and
// queues a build for it. Automatic observations (pushes and refresh syncs)
// honor the environment auto-deploy switch: when it is off the revision is
// recorded and held for a manual release instead of building.
func (c *GitHubCoordinator) observeBoundRevision(ctx context.Context, binding SourceBindingRecord, commitSHA, commitMessage, commitAuthor string, automatic bool, transition BuildTransition) error {
	revision, err := c.recordBoundRevision(ctx, binding, commitSHA, commitMessage, commitAuthor, transition)
	if err != nil {
		return err
	}
	if automatic {
		autoDeploy, err := c.store.EnvironmentAutoDeploy(ctx, binding.EnvironmentID)
		if err != nil {
			return err
		}
		if !autoDeploy {
			slog.InfoContext(ctx, "github revision recorded; auto-deploy is off", "service_id", binding.ServiceID, "environment_id", binding.EnvironmentID, "tracked_ref", binding.TrackedRef, "commit_sha", revision.CommitSHA, "source_revision_id", revision.ID)
			return nil
		}
	}
	return c.queueBoundRevisionBuild(ctx, binding, revision, transition)
}

func (c *GitHubCoordinator) recordBoundRevision(ctx context.Context, binding SourceBindingRecord, commitSHA, commitMessage, commitAuthor string, transition BuildTransition) (SourceRevisionRecord, error) {
	return c.store.ObserveSourceRevision(ctx, SourceRevisionRecord{
		SourceBindingID:              binding.ID,
		ServiceID:                    binding.ServiceID,
		Provider:                     binding.Provider,
		ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
		TrackedRef:                   binding.TrackedRef,
		CommitSHA:                    commitSHA,
		CommitMessage:                strings.TrimSpace(commitMessage),
		CommitAuthor:                 strings.TrimSpace(commitAuthor),
		ObservedAt:                   time.Now().UTC(),
	}, transition)
}

func (c *GitHubCoordinator) queueBoundRevisionBuild(ctx context.Context, binding SourceBindingRecord, revision SourceRevisionRecord, transition BuildTransition) error {
	owner, repo, err := SplitGitHubRepositorySelector(binding.RepositorySelector)
	if err != nil {
		return err
	}
	installationID := providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID)
	var pendingSnapshot SourceSnapshotRecord
	if _, err := c.store.SourceSnapshotByRevisionID(ctx, revision.ID); errors.Is(err, sql.ErrNoRows) {
		stream, size, err := c.client.FetchArchiveStream(ctx, owner, repo, revision.CommitSHA, installationID)
		if err != nil {
			return err
		}
		digest, objectKey, storedSize, err := c.store.StoreSourceArchiveFromReader(ctx, stream, size)
		_ = stream.Close()
		if err != nil {
			return err
		}
		pendingSnapshot = SourceSnapshotRecord{
			SourceRevisionID:             revision.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			CommitSHA:                    revision.CommitSHA,
			Digest:                       digest,
			ObjectKey:                    objectKey,
			ArchiveSizeBytes:             storedSize,
			Ready:                        true,
			FetchedAt:                    sql.NullTime{Time: time.Now().UTC(), Valid: true},
		}
	} else if err != nil {
		return err
	}

	queued, err := c.delivery.QueueSourceBuild(ctx, binding, revision.CommitSHA, pendingSnapshot, transition)
	if err != nil {
		return err
	}
	if queued.Superseded {
		if queued.PendingPredecessor {
			// Early successor, not a stale redelivery: keep the work item
			// alive so the successor is re-evaluated after its predecessor
			// lands. Completing it here would drop the push forever.
			return errSourcePushPredecessorPending
		}
		slog.InfoContext(ctx, "github revision superseded by newer observed commit; skipping", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", revision.CommitSHA, "source_revision_id", revision.ID)
		return nil
	}
	if queued.Reused {
		slog.InfoContext(ctx, "github build reused existing image", "service_id", binding.ServiceID, "build_id", queued.BuildID, "deployment_id", queued.DeploymentID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", revision.CommitSHA, "source_revision_id", revision.ID)
		return nil
	}
	slog.InfoContext(ctx, "github build queued", "service_id", binding.ServiceID, "build_id", queued.BuildID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", revision.CommitSHA, "source_revision_id", revision.ID)
	return nil
}

func (c *GitHubCoordinator) refreshRepositorySnapshot(ctx context.Context, owner, repo string) error {
	return c.catalog.RefreshRepositorySnapshot(ctx, owner, repo)
}

func ScopeExternalID(installationID int64) string {
	if installationID <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", installationID)
}

func providerScopeExternalIDToInstallationID(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
