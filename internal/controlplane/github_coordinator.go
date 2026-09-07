package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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
	delivery   *deliverycore.Delivery
	catalog    *GitHubCatalog
	client     *GitHubClient
	emitter    *LogEmitter
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

func NewGitHubCoordinator(store *Store, delivery *deliverycore.Delivery, catalog *GitHubCatalog, client *GitHubClient, staleAfter time.Duration, opts ...GitHubCoordinatorOption) *GitHubCoordinator {
	if store == nil || delivery == nil || catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	c := &GitHubCoordinator{
		store:      store,
		delivery:   delivery,
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

func (c *GitHubCoordinator) ClaimNextWorkItem(ctx context.Context, processorID string, staleAfter time.Duration) (deliverycore.SourceWorkItemRecord, error) {
	s := c.store
	var rec deliverycore.SourceWorkItemRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := deliverycore.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if staleAfter > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE source_work_items
				    SET state = $1,
				        processor_id = '',
				        updated_at = $2
				  WHERE state = $3
				    AND updated_at < $4`,
				deliverycore.SourceWorkStatePending, now, deliverycore.SourceWorkStateProcessing, now.Add(-staleAfter),
			); err != nil {
				return err
			}
		}
		row := tx.QueryRowContext(ctx,
			`SELECT id, kind, state, processor_id, idempotency_key, service_id, spec_revision, provider,
			        provider_repository_external_id, provider_scope_external_id, tracked_ref, commit_sha,
			        commit_message, commit_author, last_error, attempt_count, available_at, created_at, updated_at
			   FROM source_work_items
			  WHERE state = $1
			    AND available_at <= $2
			  ORDER BY available_at ASC, created_at ASC, id ASC
			  LIMIT 1`,
			deliverycore.SourceWorkStatePending, now,
		)
		if err := row.Scan(
			&rec.ID,
			&rec.Kind,
			&rec.State,
			&rec.ProcessorID,
			&rec.IdempotencyKey,
			&rec.ServiceID,
			&rec.SpecRevision,
			&rec.Provider,
			&rec.ProviderRepositoryExternalID,
			&rec.ProviderScopeExternalID,
			&rec.TrackedRef,
			&rec.CommitSHA,
			&rec.CommitMessage,
			&rec.CommitAuthor,
			&rec.LastError,
			&rec.AttemptCount,
			&rec.AvailableAt,
			&rec.CreatedAt,
			&rec.UpdatedAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec.State = deliverycore.SourceWorkStateProcessing
		rec.ProcessorID = processorID
		rec.UpdatedAt = now
		_, err = tx.ExecContext(ctx,
			`UPDATE source_work_items
			    SET state = $1,
			        processor_id = $2,
			        updated_at = $3
			  WHERE id = $4`,
			rec.State, rec.ProcessorID, rec.UpdatedAt, rec.ID,
		)
		return err
	})
	if err != nil {
		return deliverycore.SourceWorkItemRecord{}, err
	}
	return rec, nil
}

func (c *GitHubCoordinator) CompleteWorkItem(ctx context.Context, id, processorID string) error {
	result, err := c.store.db.ExecContext(ctx, `DELETE FROM source_work_items WHERE id = $1 AND state = $2 AND processor_id = $3`, id, deliverycore.SourceWorkStateProcessing, processorID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: source work item %s", errLeaseLost, id)
	}
	return nil
}

func (c *GitHubCoordinator) ReleaseWorkItem(ctx context.Context, id, processorID string, processErr error, retryAfter time.Duration) error {
	message := ""
	if processErr != nil {
		message = processErr.Error()
	}
	result, err := c.store.db.ExecContext(ctx,
		`UPDATE source_work_items
		    SET state = $1,
		        processor_id = '',
		        last_error = $2,
		        attempt_count = attempt_count + 1,
		        available_at = statement_timestamp() + $3::INT8 * INTERVAL '1 microsecond',
		        updated_at = statement_timestamp()
		  WHERE id = $4 AND state = $5 AND processor_id = $6`,
		deliverycore.SourceWorkStatePending,
		message,
		retryAfter.Microseconds(),
		id,
		deliverycore.SourceWorkStateProcessing,
		processorID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: source work item %s", errLeaseLost, id)
	}
	return nil
}

func (c *GitHubCoordinator) RecoverWorkItems(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return c.store.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE source_work_items
		    SET state = $1,
		        processor_id = '',
		        updated_at = statement_timestamp()
		  WHERE state = $3
		    AND updated_at < statement_timestamp() - $2::INT8 * INTERVAL '1 microsecond'`,
			deliverycore.SourceWorkStatePending,
			staleAfter.Microseconds(),
			deliverycore.SourceWorkStateProcessing,
		)
		return err
	})
}

func (c *GitHubCoordinator) RequestInstallationRefresh(ctx context.Context, installationID int64) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	if installationID <= 0 {
		return errors.New("installation id is required")
	}
	inserted, err := c.store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
		Kind:                    deliverycore.SourceWorkKindProviderAccessChanged,
		IdempotencyKey:          fmt.Sprintf("%s:github:%d", deliverycore.SourceWorkKindProviderAccessChanged, installationID),
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
	inserted, err := c.store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
		Kind:                         deliverycore.SourceWorkKindRevisionObserved,
		IdempotencyKey:               fmt.Sprintf("%s:github:%s:%s:%s", deliverycore.SourceWorkKindRevisionObserved, repositoryExternalID, trackedRef, commitSHA),
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

func (c *GitHubCoordinator) processWorkItem(ctx context.Context, rec deliverycore.SourceWorkItemRecord) error {
	switch rec.Kind {
	case deliverycore.SourceWorkKindProviderAccessChanged:
		return c.handleProviderAccessChanged(ctx, rec.ProviderScopeExternalID)
	case deliverycore.SourceWorkKindSourceSpecChanged:
		return c.syncServiceSource(ctx, rec.ServiceID, rec.SpecRevision)
	case deliverycore.SourceWorkKindRevisionObserved:
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
		if _, err := c.store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
			Kind:           deliverycore.SourceWorkKindSourceSpecChanged,
			IdempotencyKey: fmt.Sprintf("%s:%s", deliverycore.SourceWorkKindSourceSpecChanged, binding.ServiceID),
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
	service, err := c.store.deliveryQueries().ServiceByIDInternalQuerier(ctx, c.store.db, serviceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if specRevision > 0 && service.SpecRevision != specRevision {
		return nil
	}
	source := deliverycore.DesiredSourceSpec(service.Spec)
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
	binding, err := c.store.upsertSourceBinding(ctx, deliverycore.SourceBindingRecord{
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
	if binding.AccessState != deliverycore.SourceAccessStateAvailable {
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

func (c *GitHubCoordinator) handleRevisionObserved(ctx context.Context, rec deliverycore.SourceWorkItemRecord) error {
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
			if _, err := c.store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
				Kind:           deliverycore.SourceWorkKindSourceSpecChanged,
				IdempotencyKey: fmt.Sprintf("%s:%s:%d", deliverycore.SourceWorkKindSourceSpecChanged, binding.ServiceID, time.Now().UTC().UnixNano()),
				ServiceID:      binding.ServiceID,
			}); err != nil {
				return err
			}
			continue
		}
		if binding.AccessState != deliverycore.SourceAccessStateAvailable {
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

func (c *GitHubCoordinator) observeBoundRevision(ctx context.Context, binding deliverycore.SourceBindingRecord, commitSHA, commitMessage, commitAuthor string) error {
	owner, repo, err := splitGitHubRepositorySelector(binding.RepositorySelector)
	if err != nil {
		return err
	}
	installationID := providerScopeExternalIDToInstallationID(binding.ProviderScopeExternalID)
	var revision deliverycore.SourceRevisionRecord
	if err := c.store.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		revision, err = c.store.upsertSourceRevisionTx(ctx, tx, deliverycore.SourceRevisionRecord{
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

	var pendingSnapshot deliverycore.SourceSnapshotRecord
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
		pendingSnapshot = deliverycore.SourceSnapshotRecord{
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
	slog.InfoContext(ctx, "github build queued", "service_id", queued.Service.ID, "build_id", queued.Build.ID, "repository_selector", binding.RepositorySelector, "tracked_ref", binding.TrackedRef, "commit_sha", queued.Build.CommitSHA, "source_revision_id", revision.ID)
	if c.emitter != nil {
		c.emitter.EmitBuildf(ctx, queued.Service, queued.Build, StageBuild,
			"Queued build for commit %s on ref %s", shortSHA(queued.Build.CommitSHA), binding.TrackedRef,
		)
	}
	return nil
}

func (c *GitHubCoordinator) refreshRepositorySnapshot(ctx context.Context, owner, repo string) error {
	return c.catalog.RefreshRepositorySnapshot(ctx, owner, repo)
}

func (s *Store) upsertSourceBinding(ctx context.Context, rec deliverycore.SourceBindingRecord) (deliverycore.SourceBindingRecord, error) {
	var out deliverycore.SourceBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.upsertSourceBindingTx(ctx, tx, rec)
		return err
	})
	if err != nil {
		return deliverycore.SourceBindingRecord{}, err
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
