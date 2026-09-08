package source

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type GitHubCatalog struct {
	store  Store
	client *GitHubClient
}

func NewGitHubCatalog(store Store, client *GitHubClient) *GitHubCatalog {
	if store == nil || client == nil || !client.Enabled() {
		return nil
	}
	return &GitHubCatalog{store: store, client: client}
}

func (c *GitHubCatalog) Enabled() bool {
	return c != nil && c.store != nil && c.client != nil && c.client.Enabled()
}

func (c *GitHubCatalog) RepositoryView(ctx context.Context, owner, repo string, installationID int64) (GitHubRepositoryView, error) {
	if !c.Enabled() {
		return GitHubRepositoryView{}, errors.New("github integration disabled")
	}
	owner, repo, err := normalizeRepositoryRef(owner, repo)
	if err != nil {
		return GitHubRepositoryView{}, err
	}

	snapshot, snapshotErr := c.store.GitHubRepositorySnapshotByName(ctx, owner, repo)
	if snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
		return GitHubRepositoryView{}, snapshotErr
	}

	if installationID > 0 {
		rec, err := c.store.GitHubRepositoryGrantByName(ctx, installationID, owner, repo)
		switch {
		case err == nil:
			return GitHubRepositoryView{
				RepositoryID:   rec.RepositoryID,
				Owner:          rec.Owner,
				Repo:           rec.Repo,
				FullName:       rec.FullName,
				Private:        rec.Private,
				DefaultBranch:  rec.DefaultBranch,
				InstallationID: rec.InstallationID,
				AccessState:    SourceAccessStateAvailable,
				GrantUpdatedAt: rec.UpdatedAt,
			}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return GitHubRepositoryView{}, err
		case snapshotErr == nil:
			view := repositoryViewFromSnapshot(snapshot, installationID)
			if snapshot.Deleted {
				view.AccessState = SourceAccessStateRepositoryDeleted
			} else if snapshot.Private {
				view.AccessState = SourceAccessStateAccessRevoked
			} else {
				view.AccessState = SourceAccessStateAvailable
			}
			return view, nil
		default:
			return emptyGitHubRepositoryView(owner, repo, installationID, SourceAccessStateInstallationRequired), nil
		}
	}

	grants, err := c.store.ListGitHubRepositoryGrantsByName(ctx, owner, repo)
	if err != nil {
		return GitHubRepositoryView{}, err
	}
	if len(grants) > 0 {
		rec := grants[0]
		return GitHubRepositoryView{
			RepositoryID:   rec.RepositoryID,
			Owner:          rec.Owner,
			Repo:           rec.Repo,
			FullName:       rec.FullName,
			Private:        rec.Private,
			DefaultBranch:  rec.DefaultBranch,
			InstallationID: rec.InstallationID,
			AccessState:    SourceAccessStateAvailable,
			GrantUpdatedAt: rec.UpdatedAt,
		}, nil
	}
	if snapshotErr == nil {
		view := repositoryViewFromSnapshot(snapshot, 0)
		if snapshot.Deleted {
			view.AccessState = SourceAccessStateRepositoryDeleted
		} else if snapshot.Private {
			view.AccessState = SourceAccessStateInstallationRequired
		} else {
			view.AccessState = SourceAccessStateAvailable
		}
		return view, nil
	}
	return emptyGitHubRepositoryView(owner, repo, 0, SourceAccessStateInstallationRequired), nil
}

func (c *GitHubCatalog) ResolveRepositoryView(ctx context.Context, owner, repo string) (GitHubRepositoryView, error) {
	view, err := c.RepositoryView(ctx, owner, repo, 0)
	if err != nil {
		return GitHubRepositoryView{}, err
	}
	if view.AccessState == SourceAccessStateAvailable {
		return view, nil
	}

	installation, err := c.client.GetRepositoryInstallation(ctx, owner, repo)
	switch {
	case err == nil:
		if err := c.RefreshInstallation(ctx, installation); err != nil {
			return GitHubRepositoryView{}, err
		}
		return c.RepositoryView(ctx, owner, repo, 0)
	case isGitHubAPINotFound(err):
	case err != nil:
		return GitHubRepositoryView{}, err
	}

	if err := c.RefreshRepositorySnapshot(ctx, owner, repo); err != nil && !isGitHubRepositoryProbeUnavailable(err) {
		return GitHubRepositoryView{}, err
	}

	return c.RepositoryView(ctx, owner, repo, 0)
}

func isGitHubAPINotFound(err error) bool {
	var apiErr *GitHubAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}

func isGitHubRepositoryProbeUnavailable(err error) bool {
	var apiErr *GitHubAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 403 || apiErr.StatusCode == 404
}

func (c *GitHubCatalog) RefreshRepositorySnapshot(ctx context.Context, owner, repo string) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	repository, err := c.client.GetRepository(ctx, owner, repo, 0)
	if err != nil {
		return err
	}
	return c.store.UpsertGitHubRepositorySnapshot(ctx, GitHubRepositorySnapshotRecord{
		RepositoryID:  repository.RepositoryID,
		Owner:         repository.Owner,
		Repo:          repository.Repo,
		FullName:      repository.FullName,
		Private:       repository.Private,
		DefaultBranch: repository.DefaultBranch,
	})
}

func (c *GitHubCatalog) RefreshInstallation(ctx context.Context, installation githubInstallationView) error {
	if !c.Enabled() {
		return errors.New("github integration disabled")
	}
	repositories, err := c.client.ListInstallationRepositories(ctx, installation.InstallationID)
	if err != nil {
		return err
	}
	repos := make([]GitHubRepositoryRecord, 0, len(repositories))
	for _, repo := range repositories {
		repos = append(repos, GitHubRepositoryRecord{
			InstallationID: installation.InstallationID,
			RepositoryID:   repo.RepositoryID,
			Owner:          repo.Owner,
			Repo:           repo.Repo,
			FullName:       repo.FullName,
			Private:        repo.Private,
			DefaultBranch:  repo.DefaultBranch,
		})
	}
	return c.store.ReplaceGitHubInstallationRepositories(ctx, GitHubInstallationRecord{
		InstallationID: installation.InstallationID,
		AccountLogin:   installation.AccountLogin,
		AccountType:    installation.AccountType,
		TargetType:     installation.TargetType,
		Active:         true,
	}, repos)
}

func (c *GitHubCatalog) BuildRepositoryView(ctx context.Context, source *platformv1.ServiceSourceSpec) (GitHubRepositoryView, error) {
	if source == nil {
		return GitHubRepositoryView{}, errors.New("source spec is required")
	}
	if strings.TrimSpace(strings.ToLower(source.GetProvider())) != "github" {
		return GitHubRepositoryView{}, errors.New("unsupported source provider")
	}
	owner, repo, err := SplitGitHubRepositorySelector(source.GetRepositorySelector())
	if err != nil {
		return GitHubRepositoryView{}, err
	}
	view, err := c.RepositoryView(ctx, owner, repo, 0)
	if err != nil {
		return GitHubRepositoryView{}, err
	}
	if view.AccessState != SourceAccessStateAvailable {
		return GitHubRepositoryView{}, errors.New("repository is not deployable")
	}
	return view, nil
}

func repositoryViewFromSnapshot(rec GitHubRepositorySnapshotRecord, installationID int64) GitHubRepositoryView {
	return GitHubRepositoryView{
		RepositoryID:      rec.RepositoryID,
		Owner:             rec.Owner,
		Repo:              rec.Repo,
		FullName:          rec.FullName,
		Private:           rec.Private,
		DefaultBranch:     rec.DefaultBranch,
		InstallationID:    installationID,
		Deleted:           rec.Deleted,
		SnapshotUpdatedAt: rec.UpdatedAt,
	}
}

func emptyGitHubRepositoryView(owner, repo string, installationID int64, accessState string) GitHubRepositoryView {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	return GitHubRepositoryView{
		Owner:          owner,
		Repo:           repo,
		FullName:       githubFullName(owner, repo),
		InstallationID: installationID,
		AccessState:    accessState,
	}
}

func gitHubViewNeedsRefresh(view GitHubRepositoryView, installationID int64, staleAfter time.Duration, now time.Time) bool {
	if staleAfter <= 0 {
		return false
	}
	if installationID > 0 {
		if view.GrantUpdatedAt.IsZero() {
			return true
		}
		return now.Sub(view.GrantUpdatedAt) > staleAfter
	}
	if view.SnapshotUpdatedAt.IsZero() {
		return true
	}
	return now.Sub(view.SnapshotUpdatedAt) > staleAfter
}

func normalizeRepositoryRef(owner, repo string) (string, string, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	if owner == "" || repo == "" {
		return "", "", errors.New("owner and repo are required")
	}
	return owner, repo, nil
}
