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
)

var errGitHubWorkDeferred = errors.New("github work deferred")

type GitHubCoordinator struct {
	store      Store
	delivery   Delivery
	catalog    *GitHubCatalog
	client     *GitHubClient
	staleAfter time.Duration
	retryAfter time.Duration
}

func NewGitHubCoordinator(store Store, delivery Delivery, catalog *GitHubCatalog, client *GitHubClient, staleAfter time.Duration) *GitHubCoordinator {
	if store == nil || delivery == nil || catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	return &GitHubCoordinator{
		store:      store,
		delivery:   delivery,
		catalog:    catalog,
		client:     client,
		staleAfter: staleAfter,
		retryAfter: 5 * time.Second,
	}
}

func (c *GitHubCoordinator) Enabled() bool {
	return c != nil && c.store != nil && c.catalog != nil && c.client != nil
}

func (c *GitHubCoordinator) ClaimNextWorkItem(ctx context.Context, processorID string, staleAfter time.Duration) (SourceWorkItemRecord, error) {
	return c.store.ClaimNextSourceWorkItem(ctx, processorID, staleAfter)
}

func (c *GitHubCoordinator) CompleteWorkItem(ctx context.Context, id, processorID string) error {
	return c.store.CompleteSourceWorkItem(ctx, id, processorID)
}

func (c *GitHubCoordinator) ReleaseWorkItem(ctx context.Context, id, processorID string, processErr error, retryAfter time.Duration) error {
	return c.store.ReleaseSourceWorkItem(ctx, id, processorID, processErr, retryAfter)
}

func (c *GitHubCoordinator) RecoverWorkItems(ctx context.Context, staleAfter time.Duration) error {
	return c.store.RecoverSourceWorkItems(ctx, staleAfter)
}

func (c *GitHubCoordinator) RequestInstallationRefresh(ctx context.Context, installationID int64) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	if installationID <= 0 {
		return errors.New("installation id is required")
	}
	inserted, err := c.store.EnqueueSourceWorkItem(ctx, SourceWorkItemRecord{
		Kind:                    SourceWorkKindProviderAccessChanged,
		IdempotencyKey:          fmt.Sprintf("%s:github:%d", SourceWorkKindProviderAccessChanged, installationID),
		Provider:                "github",
		ProviderScopeExternalID: ScopeExternalID(installationID),
	})
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

func (c *GitHubCoordinator) ObserveRepositoryRevision(ctx context.Context, repositoryExternalID, trackedRef, commitSHA, commitMessage, commitAuthor string) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	repositoryExternalID = strings.TrimSpace(repositoryExternalID)
	trackedRef = strings.TrimSpace(trackedRef)
	commitSHA = strings.TrimSpace(commitSHA)
	if repositoryExternalID == "" || trackedRef == "" || commitSHA == "" {
		return errors.New("repository id, tracked ref, and commit sha are required")
	}
	inserted, err := c.store.EnqueueSourceWorkItem(ctx, SourceWorkItemRecord{
		Kind:                         SourceWorkKindRevisionObserved,
		IdempotencyKey:               fmt.Sprintf("%s:github:%s:%s:%s", SourceWorkKindRevisionObserved, repositoryExternalID, trackedRef, commitSHA),
		Provider:                     "github",
		ProviderRepositoryExternalID: repositoryExternalID,
		TrackedRef:                   trackedRef,
		CommitSHA:                    commitSHA,
		CommitMessage:                strings.TrimSpace(commitMessage),
		CommitAuthor:                 strings.TrimSpace(commitAuthor),
	})
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

func (c *GitHubCoordinator) processWorkItem(ctx context.Context, rec SourceWorkItemRecord) error {
	switch rec.Kind {
	case SourceWorkKindProviderAccessChanged:
		return c.handleProviderAccessChanged(ctx, rec.ProviderScopeExternalID)
	case SourceWorkKindSourceSpecChanged:
		return c.syncServiceSource(ctx, rec.ServiceID, rec.SpecRevision)
	case SourceWorkKindRevisionObserved:
		return c.handleRevisionObserved(ctx, rec)
	default:
		return nil
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
		if _, err := c.store.EnqueueSourceWorkItem(ctx, SourceWorkItemRecord{
			Kind:           SourceWorkKindSourceSpecChanged,
			IdempotencyKey: fmt.Sprintf("%s:%s", SourceWorkKindSourceSpecChanged, binding.ServiceID),
			ServiceID:      binding.ServiceID,
		}); err != nil {
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
	commitSHA, err := c.client.GetBranchHead(ctx, view.Owner, view.Repo, trackedRef, providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID))
	if err != nil {
		return err
	}
	metadata, err := c.client.GetCommitMetadata(ctx, view.Owner, view.Repo, commitSHA, providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID))
	if err != nil {
		return err
	}
	return c.observeBoundRevision(ctx, binding, commitSHA, metadata.Message, metadata.Author)
}

func (c *GitHubCoordinator) handleRevisionObserved(ctx context.Context, rec SourceWorkItemRecord) error {
	bindings, err := c.store.SourceBindingsForGitHubRepositoryAndRef(ctx, rec.ProviderRepositoryExternalID, rec.TrackedRef)
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		slog.InfoContext(ctx, "github revision had no bound services", "repository_external_id", rec.ProviderRepositoryExternalID, "tracked_ref", rec.TrackedRef, "commit_sha", rec.CommitSHA)
		return nil
	}
	for _, binding := range bindings {
		if time.Now().UTC().After(binding.FreshUntil) {
			slog.InfoContext(ctx, "github source binding stale; requesting refresh", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", rec.CommitSHA)
			if _, err := c.store.EnqueueSourceWorkItem(ctx, SourceWorkItemRecord{
				Kind:           SourceWorkKindSourceSpecChanged,
				IdempotencyKey: fmt.Sprintf("%s:%s:%d", SourceWorkKindSourceSpecChanged, binding.ServiceID, time.Now().UTC().UnixNano()),
				ServiceID:      binding.ServiceID,
			}); err != nil {
				return err
			}
			continue
		}
		if binding.AccessState != SourceAccessStateAvailable {
			slog.InfoContext(ctx, "github source binding unavailable", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", rec.CommitSHA, "access_state", binding.AccessState)
			continue
		}
		slog.InfoContext(ctx, "github revision matched bound service", "service_id", binding.ServiceID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", rec.CommitSHA)
		if err := c.observeBoundRevision(ctx, binding, rec.CommitSHA, rec.CommitMessage, rec.CommitAuthor); err != nil {
			return err
		}
	}
	return nil
}

func (c *GitHubCoordinator) observeBoundRevision(ctx context.Context, binding SourceBindingRecord, commitSHA, commitMessage, commitAuthor string) error {
	owner, repo, err := SplitGitHubRepositorySelector(binding.RepositorySelector)
	if err != nil {
		return err
	}
	installationID := providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID)
	revision, err := c.store.UpsertSourceRevision(ctx, SourceRevisionRecord{
		SourceBindingID:              binding.ID,
		ServiceID:                    binding.ServiceID,
		Provider:                     binding.Provider,
		ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
		TrackedRef:                   binding.TrackedRef,
		CommitSHA:                    commitSHA,
		CommitMessage:                strings.TrimSpace(commitMessage),
		CommitAuthor:                 strings.TrimSpace(commitAuthor),
		ObservedAt:                   time.Now().UTC(),
	})
	if err != nil {
		return err
	}

	var pendingSnapshot SourceSnapshotRecord
	if _, err := c.store.SourceSnapshotByRevisionID(ctx, revision.ID); errors.Is(err, sql.ErrNoRows) {
		archive, err := c.client.FetchArchive(ctx, owner, repo, commitSHA, installationID)
		if err != nil {
			return err
		}
		digest, objectKey, err := c.store.StoreSourceArchive(ctx, archive)
		if err != nil {
			return err
		}
		pendingSnapshot = SourceSnapshotRecord{
			SourceRevisionID:             revision.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			CommitSHA:                    commitSHA,
			Digest:                       digest,
			ObjectKey:                    objectKey,
			ArchiveSizeBytes:             int64(len(archive)),
			Ready:                        true,
			FetchedAt:                    sql.NullTime{Time: time.Now().UTC(), Valid: true},
		}
	} else if err != nil {
		return err
	}

	queued, err := c.delivery.QueueSourceBuild(ctx, binding, commitSHA, pendingSnapshot)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "github build queued", "service_id", binding.ServiceID, "build_id", queued.BuildID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", commitSHA, "source_revision_id", revision.ID)
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
