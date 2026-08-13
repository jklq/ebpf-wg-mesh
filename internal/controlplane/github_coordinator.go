package controlplane

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
	store      *Store
	catalog    *GitHubCatalog
	client     *GitHubClient
	emitter    *LogEmitter
	events     *PlatformEvents
	staleAfter time.Duration
	retryAfter time.Duration
}

// GitHubCoordinatorOption configures optional collaborators on the GitHub
// coordinator. We use the option pattern so production wiring can opt into
// behaviors (like synthetic log emission) without changing tests and stub
// constructions that do not need them.
type GitHubCoordinatorOption func(*GitHubCoordinator)

// WithGitHubCoordinatorLogEmitter wires a LogEmitter so that the coordinator
// can emit a build-stage line the moment a new commit is observed — that keeps
// the service panel showing progress while the builder is still claiming the
// job.
func WithGitHubCoordinatorLogEmitter(emitter *LogEmitter) GitHubCoordinatorOption {
	return func(c *GitHubCoordinator) {
		c.emitter = emitter
	}
}

func WithGitHubCoordinatorPlatformEvents(events *PlatformEvents) GitHubCoordinatorOption {
	return func(c *GitHubCoordinator) {
		c.events = events
	}
}

func NewGitHubCoordinator(store *Store, catalog *GitHubCatalog, client *GitHubClient, staleAfter time.Duration, opts ...GitHubCoordinatorOption) *GitHubCoordinator {
	if store == nil || catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	c := &GitHubCoordinator{
		store:      store,
		catalog:    catalog,
		client:     client,
		staleAfter: staleAfter,
		retryAfter: 5 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

func (c *GitHubCoordinator) Enabled() bool {
	return c != nil && c.store != nil && c.catalog != nil && c.client != nil
}

func (c *GitHubCoordinator) RequestInstallationRefresh(ctx context.Context, installationID int64) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	if installationID <= 0 {
		return errors.New("installation id is required")
	}
	inserted, err := c.store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
		Kind:                    sourceWorkKindProviderAccessChanged,
		IdempotencyKey:          fmt.Sprintf("%s:github:%d", sourceWorkKindProviderAccessChanged, installationID),
		Provider:                "github",
		ProviderScopeExternalID: scopeExternalID(installationID),
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
	inserted, err := c.store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
		Kind:                         sourceWorkKindRevisionObserved,
		IdempotencyKey:               fmt.Sprintf("%s:github:%s:%s:%s", sourceWorkKindRevisionObserved, repositoryExternalID, trackedRef, commitSHA),
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

func (c *GitHubCoordinator) processWorkItem(ctx context.Context, rec sourceWorkItemRecord) error {
	switch rec.Kind {
	case sourceWorkKindProviderAccessChanged:
		return c.handleProviderAccessChanged(ctx, rec.ProviderScopeExternalID)
	case sourceWorkKindSourceSpecChanged:
		return c.syncServiceSource(ctx, rec.ServiceID, rec.SpecRevision)
	case sourceWorkKindRevisionObserved:
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
	bindings, err := c.store.sourceBindingsForProviderScope(ctx, "github", providerScopeExternalID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if _, err := c.store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
			Kind:           sourceWorkKindSourceSpecChanged,
			IdempotencyKey: fmt.Sprintf("%s:%s", sourceWorkKindSourceSpecChanged, binding.ServiceID),
			ServiceID:      binding.ServiceID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *GitHubCoordinator) refreshInstallation(ctx context.Context, installationID int64) error {
	rec, err := c.store.githubInstallationByID(ctx, installationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		rec = githubInstallationRecord{
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
	service, err := c.store.serviceByIDInternalQuerier(ctx, c.store.db, serviceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if specRevision > 0 && service.SpecRevision != specRevision {
		return nil
	}
	source := desiredSourceSpec(service.Spec)
	if source == nil {
		return nil
	}
	if strings.TrimSpace(strings.ToLower(source.GetProvider())) != "github" {
		return nil
	}
	owner, repo, err := splitGitHubRepositorySelector(source.GetRepositorySelector())
	if err != nil {
		return err
	}
	installationID, err := c.store.projectGitHubRepositoryInstallation(ctx, service.ProjectID, owner, repo)
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
	binding, err := c.store.upsertSourceBinding(ctx, sourceBindingRecord{
		ServiceID:                    service.ID,
		ProjectID:                    service.ProjectID,
		Provider:                     "github",
		RepositorySelector:           sourceRepositorySelector(source),
		TrackedRef:                   trackedRef,
		ProviderRepositoryExternalID: fmt.Sprintf("%d", view.RepositoryID),
		ProviderScopeExternalID:      scopeExternalID(view.InstallationID),
		AccessState:                  sourceAccessStateFromGitHubView(view),
		BuildRecipe:                  buildRecipeFromSourceSpec(source),
		ResolvedAt:                   now,
		FreshUntil:                   now.Add(c.staleAfter),
	})
	if err != nil {
		return err
	}
	if binding.AccessState != sourceAccessStateAvailable {
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

func (c *GitHubCoordinator) handleRevisionObserved(ctx context.Context, rec sourceWorkItemRecord) error {
	bindings, err := c.store.sourceBindingsForGitHubRepositoryAndRef(ctx, rec.ProviderRepositoryExternalID, rec.TrackedRef)
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
			if _, err := c.store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
				Kind:           sourceWorkKindSourceSpecChanged,
				IdempotencyKey: fmt.Sprintf("%s:%s:%d", sourceWorkKindSourceSpecChanged, binding.ServiceID, time.Now().UTC().UnixNano()),
				ServiceID:      binding.ServiceID,
			}); err != nil {
				return err
			}
			continue
		}
		if binding.AccessState != sourceAccessStateAvailable {
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

func (c *GitHubCoordinator) observeBoundRevision(ctx context.Context, binding sourceBindingRecord, commitSHA, commitMessage, commitAuthor string) error {
	owner, repo, err := splitGitHubRepositorySelector(binding.RepositorySelector)
	if err != nil {
		return err
	}
	installationID := providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID)
	// We capture the service + build so we can emit a synthetic log line
	// *after* the transaction commits. Emitting inside the tx would risk
	// posting a line for work that actually rolled back, so we thread the
	// values out via closure captures.
	var (
		committedService serviceRecord
		committedBuild   buildRunRecord
		committed        bool
		revision         sourceRevisionRecord
	)
	if err := c.store.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		revision, err = c.store.upsertSourceRevisionTx(ctx, tx, sourceRevisionRecord{
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
		return err
	}); err != nil {
		return err
	}

	var pendingSnapshot sourceSnapshotRecord
	if _, err := c.store.sourceSnapshotByRevisionID(ctx, revision.ID); errors.Is(err, sql.ErrNoRows) {
		// Network download, validation, and object storage deliberately happen
		// outside the serializable CockroachDB transaction.
		archive, err := c.client.FetchArchive(ctx, owner, repo, commitSHA, installationID)
		if err != nil {
			return err
		}
		digest, objectKey, err := c.store.storeSourceArchive(ctx, archive)
		if err != nil {
			return err
		}
		pendingSnapshot = sourceSnapshotRecord{
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

	if err := c.store.withTx(ctx, func(tx *sql.Tx) error {
		revision, err := c.store.sourceRevisionByBindingAndCommitTx(ctx, tx, binding.ID, commitSHA)
		if err != nil {
			return err
		}
		snapshot, err := c.store.sourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
		if errors.Is(err, sql.ErrNoRows) {
			if pendingSnapshot.ObjectKey == "" {
				return err
			}
			snapshot, err = c.store.upsertSourceSnapshotTx(ctx, tx, pendingSnapshot)
		}
		if err != nil {
			return err
		}
		service, err := c.store.serviceByIDInternalQuerier(ctx, tx, binding.ServiceID)
		if err != nil {
			return err
		}
		build, err := c.store.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe, deploymentActor{Kind: deploymentCauseWebhook})
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "github build queued", "service_id", service.ID, "build_id", build.ID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", revision.CommitSHA, "source_revision_id", revision.ID, "source_snapshot_id", snapshot.ID)
		committedService = service
		committedBuild = build
		committed = true
		return nil
	}); err != nil {
		return err
	}
	if committed && c.emitter != nil {
		c.emitter.EmitBuildf(ctx, committedService, committedBuild, StageBuild,
			"Queued build for commit %s on ref %s", shortSHA(committedBuild.CommitSHA), binding.TrackedRef,
		)
	}
	if committed {
		c.events.Publish(committedService.EnvironmentID)
	}
	return nil
}

func (c *GitHubCoordinator) refreshRepositorySnapshot(ctx context.Context, owner, repo string) error {
	return c.catalog.RefreshRepositorySnapshot(ctx, owner, repo)
}

func (s *Store) upsertSourceBinding(ctx context.Context, rec sourceBindingRecord) (sourceBindingRecord, error) {
	var out sourceBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.upsertSourceBindingTx(ctx, tx, rec)
		return err
	})
	if err != nil {
		return sourceBindingRecord{}, err
	}
	return out, nil
}

func scopeExternalID(installationID int64) string {
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
