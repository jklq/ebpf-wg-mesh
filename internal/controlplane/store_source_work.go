package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *Store) enqueueSourceWorkItem(ctx context.Context, rec sourceWorkItemRecord) (bool, error) {
	inserted := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		inserted, err = s.enqueueSourceWorkItemTx(ctx, tx, rec)
		return err
	})
	return inserted, err
}

func (s *Store) enqueueSourceWorkItemTx(ctx context.Context, tx *sql.Tx, rec sourceWorkItemRecord) (bool, error) {
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		rec.ID = mustID()
	}
	if rec.AvailableAt.IsZero() {
		rec.AvailableAt = now
	}
	rec.State = sourceWorkStatePending
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

func toProtoSourceAccessState(value string) platformv1.SourceAccessState {
	switch strings.TrimSpace(value) {
	case sourceAccessStateAvailable:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE
	case sourceAccessStateInstallationRequired:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED
	case sourceAccessStateAccessRevoked:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_ACCESS_REVOKED
	case sourceAccessStateRepositoryDeleted:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_REPOSITORY_DELETED
	default:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_UNSPECIFIED
	}
}

func sourceAccessStateFromGitHubView(view GitHubRepositoryView) string {
	switch strings.TrimSpace(view.AccessState) {
	case sourceAccessStateAvailable:
		return sourceAccessStateAvailable
	case sourceAccessStateAccessRevoked:
		return sourceAccessStateAccessRevoked
	case sourceAccessStateRepositoryDeleted:
		return sourceAccessStateRepositoryDeleted
	default:
		return sourceAccessStateInstallationRequired
	}
}

func buildRecipeFromSourceSpec(source *platformv1.ServiceSourceSpec) *platformv1.BuildRecipe {
	if source == nil {
		return nil
	}
	return cloneBuildRecipe(source.GetBuildRecipe())
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

func buildJobSourceFromRecord(rec buildRunRecord) *platformv1.BuildJobSource {
	if rec.SourceRevisionID == "" && rec.SourceSnapshotID == "" {
		return nil
	}
	return &platformv1.BuildJobSource{
		SourceRevisionId:     rec.SourceRevisionID,
		SourceSnapshotId:     rec.SourceSnapshotID,
		SourceSnapshotDigest: rec.SourceSnapshotDigest,
		BuildRecipe:          cloneBuildRecipe(rec.BuildRecipe),
	}
}

func ensureReadySnapshot(snapshot sourceSnapshotRecord) error {
	if snapshot.ID == "" {
		return sql.ErrNoRows
	}
	if !snapshot.Ready || snapshot.ArchiveSizeBytes <= 0 {
		return fmt.Errorf("snapshot %s is not ready", snapshot.ID)
	}
	return nil
}

func (s *Store) latestSourceRevisionByBindingIDTx(ctx context.Context, q serviceQueryer, sourceBindingID string) (sourceRevisionRecord, error) {
	var rec sourceRevisionRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_binding_id, service_id, provider, provider_repository_external_id,
		        tracked_ref, commit_sha, commit_message, commit_author, observed_at, created_at
		   FROM source_revisions
		  WHERE source_binding_id = $1
		  ORDER BY observed_at DESC, created_at DESC, id DESC
		  LIMIT 1`,
		sourceBindingID,
	).Scan(
		&rec.ID,
		&rec.SourceBindingID,
		&rec.ServiceID,
		&rec.Provider,
		&rec.ProviderRepositoryExternalID,
		&rec.TrackedRef,
		&rec.CommitSHA,
		&rec.CommitMessage,
		&rec.CommitAuthor,
		&rec.ObservedAt,
		&rec.CreatedAt,
	)
	if err != nil {
		return sourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *Store) loadServiceSourceSummaryQuerier(ctx context.Context, q serviceQueryer, spec *platformv1.ServiceSpec, serviceID string) (*platformv1.ServiceSourceSummary, error) {
	if spec == nil || spec.GetSource() == nil {
		return nil, nil
	}
	if image := spec.GetSource().GetImage(); image != nil {
		return buildSourceSummary(spec), nil
	}
	desired := desiredSourceSpec(spec)
	if desired == nil {
		return nil, nil
	}
	binding, err := s.sourceBindingByServiceIDQuerier(ctx, q, serviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, nil, nil, nil), nil
	case err != nil:
		return nil, err
	}
	revision, err := s.latestSourceRevisionByBindingIDTx(ctx, q, binding.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, nil, nil), nil
	case err != nil:
		return nil, err
	}
	snapshot, err := s.sourceSnapshotByRevisionIDTx(ctx, q, revision.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, &revision, nil), nil
	case err != nil:
		return nil, err
	default:
		return toProtoSourceStateSummary(desired, &binding, &revision, &snapshot), nil
	}
}

func (s *Store) enqueueSourceSpecChangedTx(ctx context.Context, tx *sql.Tx, serviceID string, specRevision int64, force bool) error {
	if serviceID == "" {
		return errors.New("service id is required")
	}
	key := fmt.Sprintf("%s:%s:%d", sourceWorkKindSourceSpecChanged, serviceID, specRevision)
	if force {
		key = fmt.Sprintf("%s:%s:%d:%d", sourceWorkKindSourceSpecChanged, serviceID, specRevision, time.Now().UTC().UnixNano())
	}
	_, err := s.enqueueSourceWorkItemTx(ctx, tx, sourceWorkItemRecord{
		Kind:           sourceWorkKindSourceSpecChanged,
		IdempotencyKey: key,
		ServiceID:      serviceID,
		SpecRevision:   specRevision,
	})
	return err
}
