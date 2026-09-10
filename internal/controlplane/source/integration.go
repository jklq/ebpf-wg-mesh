package source

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type Store interface {
	GitHubRepositorySnapshotByName(context.Context, string, string) (GitHubRepositorySnapshotRecord, error)
	GitHubRepositoryGrantByName(context.Context, int64, string, string) (GitHubRepositoryRecord, error)
	ListGitHubRepositoryGrantsByName(context.Context, string, string) ([]GitHubRepositoryRecord, error)
	UpsertGitHubRepositorySnapshot(context.Context, GitHubRepositorySnapshotRecord) error
	ReplaceGitHubInstallationRepositories(context.Context, GitHubInstallationRecord, []GitHubRepositoryRecord) error
	LinkProjectGitHubRepository(context.Context, string, string, GitHubRepositoryView) error
	ProjectGitHubRepositoryInstallation(context.Context, string, string, string) (int64, error)
	UpsertGitHubInstallation(context.Context, GitHubInstallationRecord) error
	DeactivateGitHubInstallation(context.Context, int64) error
	MarkGitHubRepositorySnapshotDeleted(context.Context, string, string) error
	EnqueueGitHubWebhookDelivery(context.Context, string, string, []byte) (bool, error)
	ClaimNextGitHubWebhookDelivery(context.Context, string, time.Duration) (GitHubWebhookDeliveryRecord, error)
	CompleteGitHubWebhookDelivery(context.Context, string, string, error) error
	RecoverGitHubWebhookDeliveries(context.Context, time.Duration) error
	ListActiveGitHubInstallationIDs(context.Context) ([]int64, error)

	ClaimNextSourceWorkItem(context.Context, string, time.Duration) (SourceWorkItemRecord, error)
	CompleteSourceWorkItem(context.Context, string, string) error
	ReleaseSourceWorkItem(context.Context, string, string, error, time.Duration) error
	RecoverSourceWorkItems(context.Context, time.Duration) error
	EnqueueSourceWorkItem(context.Context, SourceWorkItemRecord) (bool, error)
	GitHubInstallationByID(context.Context, int64) (GitHubInstallationRecord, error)
	SourceBindingsForProviderScope(context.Context, string, string) ([]SourceBindingRecord, error)
	SourceBindingsForGitHubRepositoryAndRef(context.Context, string, string) ([]SourceBindingRecord, error)
	ServiceSnapshot(context.Context, string) (Service, error)
	UpsertSourceBinding(context.Context, SourceBindingRecord) (SourceBindingRecord, error)
	UpsertSourceRevision(context.Context, SourceRevisionRecord) (SourceRevisionRecord, error)
	SourceSnapshotByRevisionID(context.Context, string) (SourceSnapshotRecord, error)
	StoreSourceArchive(context.Context, []byte) (string, string, error)
}

type Delivery interface {
	QueueSourceBuild(context.Context, SourceBindingRecord, string, SourceSnapshotRecord) (QueuedBuild, error)
	RecoverExpiredBuilds(context.Context, time.Duration) error
}

func sourceAccessStateFromGitHubView(view GitHubRepositoryView) string {
	switch strings.TrimSpace(view.AccessState) {
	case SourceAccessStateAvailable:
		return SourceAccessStateAvailable
	case SourceAccessStateAccessRevoked:
		return SourceAccessStateAccessRevoked
	case SourceAccessStateRepositoryDeleted:
		return SourceAccessStateRepositoryDeleted
	default:
		return SourceAccessStateInstallationRequired
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	spread := base / 5
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func buildRecipeFromSourceSpec(spec *platformv1.ServiceSourceSpec) *platformv1.BuildRecipe {
	if spec == nil {
		return nil
	}
	return CloneBuildRecipe(spec.GetBuildRecipe())
}

func sourceRepositorySelector(spec *platformv1.ServiceSourceSpec) string {
	if spec == nil {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(spec.GetRepositorySelector()))
}

func sourceTrackedRef(spec *platformv1.ServiceSourceSpec, defaultRef string) string {
	if spec == nil {
		return strings.TrimSpace(defaultRef)
	}
	if ref := strings.TrimSpace(spec.GetTrackedRef()); ref != "" {
		return ref
	}
	return strings.TrimSpace(defaultRef)
}

func SplitGitHubRepositorySelector(selector string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(selector), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid github repository selector %q", selector)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func githubFullName(owner, repo string) string {
	return strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(repo))
}
