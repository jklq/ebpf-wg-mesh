package source

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *SQLStore) UpsertSourceBinding(ctx context.Context, rec SourceBindingRecord) (SourceBindingRecord, error) {
	var out SourceBindingRecord
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = s.UpsertSourceBindingTx(ctx, tx, rec)
		return err
	})
	return out, err
}

func (s *SQLStore) UpsertSourceRevision(ctx context.Context, rec SourceRevisionRecord) (SourceRevisionRecord, error) {
	var out SourceRevisionRecord
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = s.UpsertSourceRevisionTx(ctx, tx, rec)
		return err
	})
	return out, err
}

func (s *SQLStore) UpsertSourceBindingTx(ctx context.Context, tx *sql.Tx, rec SourceBindingRecord) (SourceBindingRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = uuid.NewString()
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
	recipeJSON, err := MarshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return SourceBindingRecord{}, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO source_bindings(
			id, service_id, provider, repository_selector, tracked_ref,
			provider_repository_external_id, provider_scope_external_id, access_state,
			build_recipe_json, resolved_at, fresh_until, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT(service_id) DO UPDATE
		   SET provider = excluded.provider,
		       repository_selector = excluded.repository_selector,
		       tracked_ref = excluded.tracked_ref,
		       provider_repository_external_id = excluded.provider_repository_external_id,
		       provider_scope_external_id = excluded.provider_scope_external_id,
		       access_state = excluded.access_state,
		       build_recipe_json = excluded.build_recipe_json,
		       resolved_at = excluded.resolved_at,
		       fresh_until = excluded.fresh_until,
		       updated_at = excluded.updated_at`,
		rec.ID, rec.ServiceID, rec.Provider, rec.RepositorySelector, rec.TrackedRef,
		rec.ProviderRepositoryExternalID, rec.ProviderScopeExternalID, rec.AccessState,
		recipeJSON, rec.ResolvedAt, rec.FreshUntil, rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return SourceBindingRecord{}, err
	}
	return s.SourceBindingByServiceIDQuerier(ctx, tx, rec.ServiceID)
}

func (s *SQLStore) SourceBindingByServiceID(ctx context.Context, serviceID string) (SourceBindingRecord, error) {
	return s.SourceBindingByServiceIDQuerier(ctx, s.db, serviceID)
}

func (s *SQLStore) EnvironmentAutoDeploy(ctx context.Context, environmentID string) (bool, error) {
	var autoDeploy bool
	err := s.db.QueryRowContext(ctx, `SELECT auto_deploy FROM environments WHERE id = $1`, strings.TrimSpace(environmentID)).Scan(&autoDeploy)
	if err != nil {
		return false, err
	}
	return autoDeploy, nil
}

func (s *SQLStore) SourceBindingsForGitHubRepositoryAndRef(ctx context.Context, repositoryExternalID, trackedRef string) ([]SourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sb.id, sb.service_id, e.project_id, s.environment_id, sb.provider, sb.repository_selector, sb.tracked_ref,
		        sb.provider_repository_external_id, sb.provider_scope_external_id, sb.access_state,
		        sb.build_recipe_json, sb.resolved_at, sb.fresh_until, sb.created_at, sb.updated_at
		   FROM source_bindings sb JOIN live_services s ON s.id = sb.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE sb.provider = 'github'
		    AND sb.provider_repository_external_id = $1
		    AND sb.tracked_ref = $2
		  ORDER BY sb.created_at ASC, sb.id ASC`,
		strings.TrimSpace(repositoryExternalID), strings.TrimSpace(trackedRef),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SourceBindingRecord
	for rows.Next() {
		var (
			rec        SourceBindingRecord
			recipeJSON []byte
		)
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		rec.BuildRecipe, err = UnmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLStore) SourceBindingsForProviderScope(ctx context.Context, provider, providerScopeExternalID string) ([]SourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sb.id, sb.service_id, e.project_id, s.environment_id, sb.provider, sb.repository_selector, sb.tracked_ref,
		        sb.provider_repository_external_id, sb.provider_scope_external_id, sb.access_state,
		        sb.build_recipe_json, sb.resolved_at, sb.fresh_until, sb.created_at, sb.updated_at
		   FROM source_bindings sb JOIN live_services s ON s.id = sb.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE sb.provider = $1
		    AND sb.provider_scope_external_id = $2
		  ORDER BY sb.created_at ASC, sb.id ASC`,
		strings.TrimSpace(provider), strings.TrimSpace(providerScopeExternalID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SourceBindingRecord
	for rows.Next() {
		var (
			rec        SourceBindingRecord
			recipeJSON []byte
		)
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		rec.BuildRecipe, err = UnmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLStore) UpsertSourceRevisionTx(ctx context.Context, tx *sql.Tx, rec SourceRevisionRecord) (SourceRevisionRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = uuid.NewString()
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
		return SourceRevisionRecord{}, err
	}
	return s.SourceRevisionByBindingAndCommitTx(ctx, tx, rec.SourceBindingID, rec.CommitSHA)
}

func (s *SQLStore) SourceRevisionByBindingAndCommit(ctx context.Context, sourceBindingID, commitSHA string) (SourceRevisionRecord, error) {
	return s.SourceRevisionByBindingAndCommitTx(ctx, s.db, sourceBindingID, commitSHA)
}

func (s *SQLStore) SourceSnapshotByRevisionID(ctx context.Context, sourceRevisionID string) (SourceSnapshotRecord, error) {
	return s.SourceSnapshotByRevisionIDTx(ctx, s.db, sourceRevisionID)
}

func (s *SQLStore) SourceSnapshotByID(ctx context.Context, snapshotID string) (SourceSnapshotRecord, error) {
	var rec SourceSnapshotRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(source_revision_id, ''), provider, provider_repository_external_id, commit_sha, digest,
		        object_key, archive_size_bytes, ready, fetched_at, created_at, updated_at
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

func (s *SQLStore) UpsertSourceSnapshotTx(ctx context.Context, tx *sql.Tx, rec SourceSnapshotRecord) (SourceSnapshotRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	result, err := tx.ExecContext(ctx,
		`INSERT INTO source_snapshots(
			id, source_revision_id, provider, provider_repository_external_id, commit_sha,
			digest, object_key, archive_size_bytes, ready, fetched_at, created_at, updated_at
		) VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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
		return s.SourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
	}
	if rec.ObjectKey != "" && rec.Digest != "" && rec.ArchiveSizeBytes > 0 {
		if err := upsertSourceArchiveObjectTx(ctx, tx, rec.ObjectKey, rec.Digest, rec.ArchiveSizeBytes); err != nil {
			return SourceSnapshotRecord{}, err
		}
	}
	return s.SourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
}

func (s *SQLStore) SourceBindingByServiceIDQuerier(ctx context.Context, q Querier, serviceID string) (SourceBindingRecord, error) {
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

func (s *SQLStore) SourceRevisionByBindingAndCommitTx(ctx context.Context, q Querier, sourceBindingID, commitSHA string) (SourceRevisionRecord, error) {
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

func (s *SQLStore) SourceSnapshotByRevisionIDTx(ctx context.Context, q Querier, sourceRevisionID string) (SourceSnapshotRecord, error) {
	var rec SourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, COALESCE(source_revision_id, ''), provider, provider_repository_external_id, commit_sha, digest,
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
		revision, revisionErr := s.SourceRevisionByIDTx(ctx, q, sourceRevisionID)
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

func (s *SQLStore) SourceRevisionByIDTx(ctx context.Context, q Querier, sourceRevisionID string) (SourceRevisionRecord, error) {
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

func (s *SQLStore) ServiceHasUnbuiltSourceRevisionTx(ctx context.Context, q Querier, serviceID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM (
			SELECT sr.id FROM source_revisions sr
			JOIN source_bindings sb ON sb.id = sr.source_binding_id
			WHERE sb.service_id = $1
			ORDER BY sr.observed_at DESC, sr.created_at DESC, sr.id DESC
			LIMIT 1
		) latest
		LEFT JOIN build_runs b ON b.source_revision_id = latest.id
		WHERE b.id IS NULL
	)`, serviceID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *SQLStore) ServicesWithUnbuiltSourceRevisionsTx(ctx context.Context, q Querier, environmentID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT s.id FROM services s
		WHERE s.environment_id = $1
		AND s.deleted_at IS NULL
		AND EXISTS (
			SELECT 1 FROM (
				SELECT sr.id FROM source_revisions sr
				JOIN source_bindings sb ON sb.id = sr.source_binding_id
				WHERE sb.service_id = s.id
				ORDER BY sr.observed_at DESC, sr.created_at DESC, sr.id DESC
				LIMIT 1
			) latest
			LEFT JOIN build_runs b ON b.source_revision_id = latest.id
			WHERE b.id IS NULL
		)
		ORDER BY s.id FOR UPDATE OF s`, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *SQLStore) LatestSourceRevisionByBindingIDTx(ctx context.Context, q Querier, sourceBindingID string) (SourceRevisionRecord, error) {
	var rec SourceRevisionRecord
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
		return SourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *SQLStore) sourceSnapshotByProviderRepoAndCommitTx(ctx context.Context, q Querier, provider, repositoryExternalID, commitSHA string) (SourceSnapshotRecord, error) {
	var rec SourceSnapshotRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, COALESCE(source_revision_id, ''), provider, provider_repository_external_id, commit_sha, digest,
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
