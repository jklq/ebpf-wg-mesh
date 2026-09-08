package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *sourcePersistence) enqueueSourceWorkItem(ctx context.Context, rec deliverycore.SourceWorkItemRecord) (bool, error) {
	inserted := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		inserted, err = s.enqueueSourceWorkItemTx(ctx, tx, rec)
		return err
	})
	return inserted, err
}

func sourceAccessStateFromGitHubView(view GitHubRepositoryView) string {
	switch strings.TrimSpace(view.AccessState) {
	case deliverycore.SourceAccessStateAvailable:
		return deliverycore.SourceAccessStateAvailable
	case deliverycore.SourceAccessStateAccessRevoked:
		return deliverycore.SourceAccessStateAccessRevoked
	case deliverycore.SourceAccessStateRepositoryDeleted:
		return deliverycore.SourceAccessStateRepositoryDeleted
	default:
		return deliverycore.SourceAccessStateInstallationRequired
	}
}

func buildRecipeFromSourceSpec(source *platformv1.ServiceSourceSpec) *platformv1.BuildRecipe {
	if source == nil {
		return nil
	}
	return deliverycore.CloneBuildRecipe(source.GetBuildRecipe())
}

func sourceRepositorySelector(source *platformv1.ServiceSourceSpec) string {
	if source == nil {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(source.GetRepositorySelector()))
}

func sourceTrackedRef(source *platformv1.ServiceSourceSpec, defaultRef string) string {
	if source == nil {
		return strings.TrimSpace(defaultRef)
	}
	trackedRef := strings.TrimSpace(source.GetTrackedRef())
	if trackedRef == "" {
		trackedRef = strings.TrimSpace(defaultRef)
	}
	return trackedRef
}

func splitGitHubRepositorySelector(selector string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(selector), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid github repository selector %q", selector)
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("invalid github repository selector %q", selector)
	}
	return owner, repo, nil
}

func buildJobSourceFromRecord(rec deliverycore.BuildRunRecord) *platformv1.BuildJobSource {
	if rec.SourceRevisionID == "" && rec.SourceSnapshotID == "" {
		return nil
	}
	return &platformv1.BuildJobSource{
		SourceRevisionId:     rec.SourceRevisionID,
		SourceSnapshotId:     rec.SourceSnapshotID,
		SourceSnapshotDigest: rec.SourceSnapshotDigest,
		BuildRecipe:          deliverycore.CloneBuildRecipe(rec.BuildRecipe),
	}
}

func (s *sourcePersistence) enqueueSourceWorkItemTx(ctx context.Context, tx *sql.Tx, rec deliverycore.SourceWorkItemRecord) (bool, error) {
	now, err := deliverycore.DatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		rec.ID = deliverycore.MustID()
	}
	if rec.AvailableAt.IsZero() {
		rec.AvailableAt = now
	}
	rec.State = deliverycore.SourceWorkStatePending
	rec.ProcessorID = ""
	rec.LastError = ""
	rec.CreatedAt = now
	rec.UpdatedAt = now
	result, err := tx.ExecContext(ctx,
		`INSERT INTO source_work_items(
			id, kind, state, processor_id, idempotency_key, service_id, spec_revision, provider,
			provider_repository_external_id, provider_scope_external_id, tracked_ref, commit_sha,
			commit_message, commit_author, last_error, attempt_count, available_at, created_at, updated_at
		) VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, '', 0, $14, $15, $15)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		rec.ID, rec.Kind, rec.State, rec.IdempotencyKey, rec.ServiceID, rec.SpecRevision, rec.Provider,
		rec.ProviderRepositoryExternalID, rec.ProviderScopeExternalID, rec.TrackedRef, rec.CommitSHA,
		rec.CommitMessage, rec.CommitAuthor, rec.AvailableAt, rec.CreatedAt,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}
