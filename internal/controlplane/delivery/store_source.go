package delivery

import (
	"context"
	"database/sql"
	"errors"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	SourceAccessStateAvailable            = "available"
	SourceAccessStateInstallationRequired = "installation_required"
	SourceAccessStateAccessRevoked        = "access_revoked"
	SourceAccessStateRepositoryDeleted    = "repository_deleted"

	SourceWorkKindSourceSpecChanged     = "source_spec_changed"
	SourceWorkKindProviderAccessChanged = "provider_access_changed"
	SourceWorkKindRevisionObserved      = "revision_observed"

	SourceWorkStatePending    = "pending"
	SourceWorkStateProcessing = "processing"
)

func CloneBuildRecipe(recipe *platformv1.BuildRecipe) *platformv1.BuildRecipe {
	if recipe == nil {
		return nil
	}
	return proto.Clone(recipe).(*platformv1.BuildRecipe)
}

func MarshalBuildRecipe(recipe *platformv1.BuildRecipe) ([]byte, error) {
	if recipe == nil {
		return []byte("{}"), nil
	}
	return protojson.Marshal(recipe)
}

func UnmarshalBuildRecipe(raw []byte) (*platformv1.BuildRecipe, error) {
	if len(raw) == 0 || string(raw) == "" || string(raw) == "null" || string(raw) == "{}" {
		return &platformv1.BuildRecipe{}, nil
	}
	recipe := &platformv1.BuildRecipe{}
	if err := protojson.Unmarshal(raw, recipe); err != nil {
		return nil, err
	}
	return recipe, nil
}

func (s *persistence) sourceBindingByServiceIDQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (SourceBindingRecord, error) {
	var (
		rec        SourceBindingRecord
		recipeJSON []byte
	)
	err := q.QueryRowContext(ctx,
		`SELECT sb.id, sb.service_id, e.project_id, s.environment_id, sb.provider, sb.repository_selector, sb.tracked_ref,
		        sb.provider_repository_external_id, sb.provider_scope_external_id, sb.access_state,
		        sb.build_recipe_json, sb.resolved_at, sb.fresh_until, sb.created_at, sb.updated_at
		   FROM source_bindings sb JOIN services s ON s.id = sb.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE sb.service_id = $1`,
		serviceID,
	).Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.ProjectID,
		&rec.EnvironmentID,
		&rec.Provider,
		&rec.RepositorySelector,
		&rec.TrackedRef,
		&rec.ProviderRepositoryExternalID,
		&rec.ProviderScopeExternalID,
		&rec.AccessState,
		&recipeJSON,
		&rec.ResolvedAt,
		&rec.FreshUntil,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		return SourceBindingRecord{}, err
	}
	rec.BuildRecipe, err = UnmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return SourceBindingRecord{}, err
	}
	return rec, nil
}

func (s *persistence) sourceRevisionByBindingAndCommitTx(ctx context.Context, q ServiceQueryer, sourceBindingID, commitSHA string) (SourceRevisionRecord, error) {
	var rec SourceRevisionRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_binding_id, service_id, provider, provider_repository_external_id,
		        tracked_ref, commit_sha, commit_message, commit_author, observed_at, created_at
		   FROM source_revisions
		  WHERE source_binding_id = $1
		    AND commit_sha = $2`,
		sourceBindingID, commitSHA,
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
		return SourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *persistence) upsertSourceSnapshotTx(ctx context.Context, tx *sql.Tx, rec SourceSnapshotRecord) (SourceSnapshotRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = MustID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	result, err := tx.ExecContext(ctx,
		`INSERT INTO source_snapshots(
			id, source_revision_id, provider, provider_repository_external_id, commit_sha,
			digest, object_key, archive_size_bytes, ready, fetched_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT(source_revision_id) DO NOTHING`,
		rec.ID, rec.SourceRevisionID, rec.Provider, rec.ProviderRepositoryExternalID, rec.CommitSHA,
		rec.Digest, rec.ObjectKey, rec.ArchiveSizeBytes, rec.Ready, nullableTime(rec.FetchedAt), rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return SourceSnapshotRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SourceSnapshotRecord{}, err
	}
	if rows == 0 {
		return s.sourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
	}
	return s.sourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func (s *persistence) sourceSnapshotByProviderRepoAndCommitTx(ctx context.Context, q ServiceQueryer, provider, repositoryExternalID, commitSHA string) (SourceSnapshotRecord, error) {
	var rec SourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
		        object_key, archive_size_bytes, ready, fetched_at, created_at, updated_at
		   FROM source_snapshots
		  WHERE provider = $1
		    AND provider_repository_external_id = $2
		    AND commit_sha = $3`,
		provider, repositoryExternalID, commitSHA,
	).Scan(
		&rec.ID,
		&rec.SourceRevisionID,
		&rec.Provider,
		&rec.ProviderRepositoryExternalID,
		&rec.CommitSHA,
		&rec.Digest,
		&rec.ObjectKey,
		&rec.ArchiveSizeBytes,
		&rec.Ready,
		&rec.FetchedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		return SourceSnapshotRecord{}, err
	}
	return rec, nil
}

func (s *persistence) sourceSnapshotByRevisionIDTx(ctx context.Context, q ServiceQueryer, sourceRevisionID string) (SourceSnapshotRecord, error) {
	var rec SourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
		        object_key, archive_size_bytes, ready, fetched_at, created_at, updated_at
		   FROM source_snapshots
		  WHERE source_revision_id = $1`,
		sourceRevisionID,
	).Scan(
		&rec.ID,
		&rec.SourceRevisionID,
		&rec.Provider,
		&rec.ProviderRepositoryExternalID,
		&rec.CommitSHA,
		&rec.Digest,
		&rec.ObjectKey,
		&rec.ArchiveSizeBytes,
		&rec.Ready,
		&rec.FetchedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return SourceSnapshotRecord{}, err
		}
		revision, revisionErr := s.sourceRevisionByIDTx(ctx, q, sourceRevisionID)
		if revisionErr != nil {
			if errors.Is(revisionErr, sql.ErrNoRows) {
				return SourceSnapshotRecord{}, err
			}
			return SourceSnapshotRecord{}, revisionErr
		}
		return s.sourceSnapshotByProviderRepoAndCommitTx(ctx, q, revision.Provider, revision.ProviderRepositoryExternalID, revision.CommitSHA)
	}
	return rec, nil
}

func (s *persistence) sourceRevisionByIDTx(ctx context.Context, q ServiceQueryer, sourceRevisionID string) (SourceRevisionRecord, error) {
	var rec SourceRevisionRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_binding_id, service_id, provider, provider_repository_external_id,
		        tracked_ref, commit_sha, commit_message, commit_author, observed_at, created_at
		   FROM source_revisions
		  WHERE id = $1`,
		sourceRevisionID,
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
		return SourceRevisionRecord{}, err
	}
	return rec, nil
}
