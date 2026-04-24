package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	sourceAccessStateAvailable            = "available"
	sourceAccessStateInstallationRequired = "installation_required"
	sourceAccessStateAccessRevoked        = "access_revoked"
	sourceAccessStateRepositoryDeleted    = "repository_deleted"

	sourceWorkKindSourceSpecChanged     = "source_spec_changed"
	sourceWorkKindProviderAccessChanged = "provider_access_changed"
	sourceWorkKindRevisionObserved      = "revision_observed"

	sourceWorkStatePending    = "pending"
	sourceWorkStateProcessing = "processing"
)

func cloneBuildRecipe(recipe *platformv1.BuildRecipe) *platformv1.BuildRecipe {
	if recipe == nil {
		return nil
	}
	return proto.Clone(recipe).(*platformv1.BuildRecipe)
}

func marshalBuildRecipe(recipe *platformv1.BuildRecipe) ([]byte, error) {
	if recipe == nil {
		return []byte("{}"), nil
	}
	return protojson.Marshal(recipe)
}

func unmarshalBuildRecipe(raw []byte) (*platformv1.BuildRecipe, error) {
	if len(raw) == 0 || string(raw) == "" || string(raw) == "null" || string(raw) == "{}" {
		return &platformv1.BuildRecipe{}, nil
	}
	recipe := &platformv1.BuildRecipe{}
	if err := protojson.Unmarshal(raw, recipe); err != nil {
		return nil, err
	}
	return recipe, nil
}

func snapshotDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) upsertSourceBindingTx(ctx context.Context, tx *sql.Tx, rec sourceBindingRecord) (sourceBindingRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = mustID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.ResolvedAt.IsZero() {
		rec.ResolvedAt = now
	}
	if rec.FreshUntil.IsZero() {
		rec.FreshUntil = now
	}
	rec.UpdatedAt = now
	recipeJSON, err := marshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return sourceBindingRecord{}, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO source_bindings(
			id, service_id, project_id, provider, repository_selector, tracked_ref,
			provider_repository_external_id, provider_scope_external_id, access_state,
			build_recipe_json, resolved_at, fresh_until, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT(service_id) DO UPDATE
		   SET project_id = excluded.project_id,
		       provider = excluded.provider,
		       repository_selector = excluded.repository_selector,
		       tracked_ref = excluded.tracked_ref,
		       provider_repository_external_id = excluded.provider_repository_external_id,
		       provider_scope_external_id = excluded.provider_scope_external_id,
		       access_state = excluded.access_state,
		       build_recipe_json = excluded.build_recipe_json,
		       resolved_at = excluded.resolved_at,
		       fresh_until = excluded.fresh_until,
		       updated_at = excluded.updated_at`,
		rec.ID, rec.ServiceID, rec.ProjectID, rec.Provider, rec.RepositorySelector, rec.TrackedRef,
		rec.ProviderRepositoryExternalID, rec.ProviderScopeExternalID, rec.AccessState,
		recipeJSON, rec.ResolvedAt, rec.FreshUntil, rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return sourceBindingRecord{}, err
	}
	return s.sourceBindingByServiceIDQuerier(ctx, tx, rec.ServiceID)
}

func (s *Store) deleteSourceBindingTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM source_bindings WHERE service_id = $1`, serviceID)
	return err
}

func (s *Store) sourceBindingByServiceID(ctx context.Context, serviceID string) (sourceBindingRecord, error) {
	return s.sourceBindingByServiceIDQuerier(ctx, s.db, serviceID)
}

func (s *Store) sourceBindingByServiceIDQuerier(ctx context.Context, q serviceQueryer, serviceID string) (sourceBindingRecord, error) {
	var (
		rec        sourceBindingRecord
		recipeJSON []byte
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, service_id, project_id, provider, repository_selector, tracked_ref,
		        provider_repository_external_id, provider_scope_external_id, access_state,
		        build_recipe_json, resolved_at, fresh_until, created_at, updated_at
		   FROM source_bindings
		  WHERE service_id = $1`,
		serviceID,
	).Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.ProjectID,
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
		return sourceBindingRecord{}, err
	}
	rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return sourceBindingRecord{}, err
	}
	return rec, nil
}

func (s *Store) sourceBindingsForGitHubRepositoryAndRef(ctx context.Context, repositoryExternalID, trackedRef string) ([]sourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, project_id, provider, repository_selector, tracked_ref,
		        provider_repository_external_id, provider_scope_external_id, access_state,
		        build_recipe_json, resolved_at, fresh_until, created_at, updated_at
		   FROM source_bindings
		  WHERE provider = 'github'
		    AND provider_repository_external_id = $1
		    AND tracked_ref = $2
		  ORDER BY created_at ASC, id ASC`,
		strings.TrimSpace(repositoryExternalID), strings.TrimSpace(trackedRef),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sourceBindingRecord
	for rows.Next() {
		var (
			rec        sourceBindingRecord
			recipeJSON []byte
		)
		if err := rows.Scan(
			&rec.ID,
			&rec.ServiceID,
			&rec.ProjectID,
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
		); err != nil {
			return nil, err
		}
		rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) sourceBindingsForProviderScope(ctx context.Context, provider, providerScopeExternalID string) ([]sourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, project_id, provider, repository_selector, tracked_ref,
		        provider_repository_external_id, provider_scope_external_id, access_state,
		        build_recipe_json, resolved_at, fresh_until, created_at, updated_at
		   FROM source_bindings
		  WHERE provider = $1
		    AND provider_scope_external_id = $2
		  ORDER BY created_at ASC, id ASC`,
		strings.TrimSpace(provider), strings.TrimSpace(providerScopeExternalID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sourceBindingRecord
	for rows.Next() {
		var (
			rec        sourceBindingRecord
			recipeJSON []byte
		)
		if err := rows.Scan(
			&rec.ID,
			&rec.ServiceID,
			&rec.ProjectID,
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
		); err != nil {
			return nil, err
		}
		rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) upsertSourceRevisionTx(ctx context.Context, tx *sql.Tx, rec sourceRevisionRecord) (sourceRevisionRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = mustID()
	}
	if rec.ObservedAt.IsZero() {
		rec.ObservedAt = now
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO source_revisions(
			id, source_binding_id, service_id, provider, provider_repository_external_id,
			tracked_ref, commit_sha, commit_message, commit_author, observed_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT(source_binding_id, commit_sha) DO NOTHING`,
		rec.ID, rec.SourceBindingID, rec.ServiceID, rec.Provider, rec.ProviderRepositoryExternalID,
		rec.TrackedRef, rec.CommitSHA, rec.CommitMessage, rec.CommitAuthor, rec.ObservedAt, rec.CreatedAt,
	)
	if err != nil {
		return sourceRevisionRecord{}, err
	}
	return s.sourceRevisionByBindingAndCommitTx(ctx, tx, rec.SourceBindingID, rec.CommitSHA)
}

func (s *Store) sourceRevisionByBindingAndCommit(ctx context.Context, sourceBindingID, commitSHA string) (sourceRevisionRecord, error) {
	return s.sourceRevisionByBindingAndCommitTx(ctx, s.db, sourceBindingID, commitSHA)
}

func (s *Store) sourceRevisionByBindingAndCommitTx(ctx context.Context, q serviceQueryer, sourceBindingID, commitSHA string) (sourceRevisionRecord, error) {
	var rec sourceRevisionRecord
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
		return sourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *Store) upsertSourceSnapshotTx(ctx context.Context, tx *sql.Tx, rec sourceSnapshotRecord) (sourceSnapshotRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = mustID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	result, err := tx.ExecContext(ctx,
		`INSERT INTO source_snapshots(
			id, source_revision_id, provider, provider_repository_external_id, commit_sha,
			digest, archive_tgz, ready, fetched_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT(source_revision_id) DO NOTHING`,
		rec.ID, rec.SourceRevisionID, rec.Provider, rec.ProviderRepositoryExternalID, rec.CommitSHA,
		rec.Digest, rec.ArchiveTGZ, rec.Ready, nullableTime(rec.FetchedAt), rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return sourceSnapshotRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return sourceSnapshotRecord{}, err
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

func (s *Store) sourceSnapshotByProviderRepoAndCommit(ctx context.Context, provider, repositoryExternalID, commitSHA string) (sourceSnapshotRecord, error) {
	return s.sourceSnapshotByProviderRepoAndCommitTx(ctx, s.db, provider, repositoryExternalID, commitSHA)
}

func (s *Store) sourceSnapshotByProviderRepoAndCommitTx(ctx context.Context, q serviceQueryer, provider, repositoryExternalID, commitSHA string) (sourceSnapshotRecord, error) {
	var rec sourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
		        archive_tgz, ready, fetched_at, created_at, updated_at
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
		&rec.ArchiveTGZ,
		&rec.Ready,
		&rec.FetchedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		return sourceSnapshotRecord{}, err
	}
	return rec, nil
}

func (s *Store) sourceSnapshotByRevisionID(ctx context.Context, sourceRevisionID string) (sourceSnapshotRecord, error) {
	return s.sourceSnapshotByRevisionIDTx(ctx, s.db, sourceRevisionID)
}

func (s *Store) sourceSnapshotByRevisionIDTx(ctx context.Context, q serviceQueryer, sourceRevisionID string) (sourceSnapshotRecord, error) {
	var rec sourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
		        archive_tgz, ready, fetched_at, created_at, updated_at
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
		&rec.ArchiveTGZ,
		&rec.Ready,
		&rec.FetchedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return sourceSnapshotRecord{}, err
		}
		revision, revisionErr := s.sourceRevisionByIDTx(ctx, q, sourceRevisionID)
		if revisionErr != nil {
			if errors.Is(revisionErr, sql.ErrNoRows) {
				return sourceSnapshotRecord{}, err
			}
			return sourceSnapshotRecord{}, revisionErr
		}
		return s.sourceSnapshotByProviderRepoAndCommitTx(ctx, q, revision.Provider, revision.ProviderRepositoryExternalID, revision.CommitSHA)
	}
	return rec, nil
}

func (s *Store) sourceRevisionByIDTx(ctx context.Context, q serviceQueryer, sourceRevisionID string) (sourceRevisionRecord, error) {
	var rec sourceRevisionRecord
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
		return sourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *Store) sourceSnapshotByID(ctx context.Context, snapshotID string) (sourceSnapshotRecord, error) {
	var rec sourceSnapshotRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
		        archive_tgz, ready, fetched_at, created_at, updated_at
		   FROM source_snapshots
		  WHERE id = $1`,
		snapshotID,
	).Scan(
		&rec.ID,
		&rec.SourceRevisionID,
		&rec.Provider,
		&rec.ProviderRepositoryExternalID,
		&rec.CommitSHA,
		&rec.Digest,
		&rec.ArchiveTGZ,
		&rec.Ready,
		&rec.FetchedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		return sourceSnapshotRecord{}, err
	}
	return rec, nil
}
