package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func snapshotDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *sourcePersistence) upsertSourceBindingTx(ctx context.Context, tx *sql.Tx, rec deliverycore.SourceBindingRecord) (deliverycore.SourceBindingRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = deliverycore.MustID()
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
	recipeJSON, err := deliverycore.MarshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return deliverycore.SourceBindingRecord{}, err
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
		return deliverycore.SourceBindingRecord{}, err
	}
	return s.reads.deliveryQueries().SourceBindingByServiceIDQuerier(ctx, tx, rec.ServiceID)
}

func (s *sourcePersistence) deleteSourceBindingTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM source_bindings WHERE service_id = $1`, serviceID)
	return err
}

func (s *sourcePersistence) sourceBindingByServiceID(ctx context.Context, serviceID string) (deliverycore.SourceBindingRecord, error) {
	return s.reads.deliveryQueries().SourceBindingByServiceIDQuerier(ctx, s.db, serviceID)
}

func (s *sourcePersistence) sourceBindingsForGitHubRepositoryAndRef(ctx context.Context, repositoryExternalID, trackedRef string) ([]deliverycore.SourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sb.id, sb.service_id, e.project_id, s.environment_id, sb.provider, sb.repository_selector, sb.tracked_ref,
		        sb.provider_repository_external_id, sb.provider_scope_external_id, sb.access_state,
		        sb.build_recipe_json, sb.resolved_at, sb.fresh_until, sb.created_at, sb.updated_at
		   FROM source_bindings sb JOIN services s ON s.id = sb.service_id
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

	var out []deliverycore.SourceBindingRecord
	for rows.Next() {
		var (
			rec        deliverycore.SourceBindingRecord
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
		rec.BuildRecipe, err = deliverycore.UnmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *sourcePersistence) sourceBindingsForProviderScope(ctx context.Context, provider, providerScopeExternalID string) ([]deliverycore.SourceBindingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sb.id, sb.service_id, e.project_id, s.environment_id, sb.provider, sb.repository_selector, sb.tracked_ref,
		        sb.provider_repository_external_id, sb.provider_scope_external_id, sb.access_state,
		        sb.build_recipe_json, sb.resolved_at, sb.fresh_until, sb.created_at, sb.updated_at
		   FROM source_bindings sb JOIN services s ON s.id = sb.service_id
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

	var out []deliverycore.SourceBindingRecord
	for rows.Next() {
		var (
			rec        deliverycore.SourceBindingRecord
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
		rec.BuildRecipe, err = deliverycore.UnmarshalBuildRecipe(recipeJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *sourcePersistence) upsertSourceRevisionTx(ctx context.Context, tx *sql.Tx, rec deliverycore.SourceRevisionRecord) (deliverycore.SourceRevisionRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = deliverycore.MustID()
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
		return deliverycore.SourceRevisionRecord{}, err
	}
	return s.reads.deliveryQueries().SourceRevisionByBindingAndCommitTx(ctx, tx, rec.SourceBindingID, rec.CommitSHA)
}

func (s *sourcePersistence) sourceRevisionByBindingAndCommit(ctx context.Context, sourceBindingID, commitSHA string) (deliverycore.SourceRevisionRecord, error) {
	return s.reads.deliveryQueries().SourceRevisionByBindingAndCommitTx(ctx, s.db, sourceBindingID, commitSHA)
}

func (s *sourcePersistence) sourceSnapshotByProviderRepoAndCommit(ctx context.Context, provider, repositoryExternalID, commitSHA string) (deliverycore.SourceSnapshotRecord, error) {
	return s.reads.deliveryQueries().SourceSnapshotByProviderRepoAndCommitTx(ctx, s.db, provider, repositoryExternalID, commitSHA)
}

func (s *sourcePersistence) sourceSnapshotByRevisionID(ctx context.Context, sourceRevisionID string) (deliverycore.SourceSnapshotRecord, error) {
	return s.reads.deliveryQueries().SourceSnapshotByRevisionIDTx(ctx, s.db, sourceRevisionID)
}

func (s *sourcePersistence) sourceSnapshotByID(ctx context.Context, snapshotID string) (deliverycore.SourceSnapshotRecord, error) {
	var rec deliverycore.SourceSnapshotRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, source_revision_id, provider, provider_repository_external_id, commit_sha, digest,
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
		return deliverycore.SourceSnapshotRecord{}, err
	}
	return rec, nil
}

func (s *sourcePersistence) sourceSnapshotArchiveChunk(ctx context.Context, snapshotID string, offset int64, limit int) ([]byte, error) {
	if offset < 0 || limit <= 0 {
		return nil, errors.New("invalid source snapshot archive range")
	}
	snapshot, err := s.sourceSnapshotByID(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if snapshot.ObjectKey == "" {
		return nil, fmt.Errorf("snapshot %s has no source archive object", snapshot.ID)
	}
	if s.sourceArchives == nil {
		return nil, errors.New("source archive store is not configured")
	}
	return s.sourceArchives.ReadRange(ctx, snapshot.ObjectKey, offset, limit)
}
