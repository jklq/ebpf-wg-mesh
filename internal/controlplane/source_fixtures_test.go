//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"time"
)

func (s *sourcePersistence) upsertSourceSnapshotTx(ctx context.Context, tx *sql.Tx, rec deliverycore.SourceSnapshotRecord) (deliverycore.SourceSnapshotRecord, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = deliverycore.MustID()
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
		return deliverycore.SourceSnapshotRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return deliverycore.SourceSnapshotRecord{}, err
	}
	if rows == 0 {
		return s.reads.deliveryQueries().SourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
	}
	return s.reads.deliveryQueries().SourceSnapshotByRevisionIDTx(ctx, tx, rec.SourceRevisionID)
}

func sourceSummaryForTest(ctx context.Context, s *persistence, id string) (*platformv1.ServiceSourceSummary, error) {
	service, err := s.reads.deliveryQueries().ServiceByIDInternalQuerier(ctx, s.db, id)
	return service.SourceSummary, err
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}
