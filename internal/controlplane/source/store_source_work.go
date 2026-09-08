package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ebof-wg-mesh/internal/controlplane/dbtx"
)

func (s *SQLStore) ClaimNextSourceWorkItem(ctx context.Context, processorID string, staleAfter time.Duration) (SourceWorkItemRecord, error) {
	var rec SourceWorkItemRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if staleAfter > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE source_work_items SET state = $1, processor_id = '', updated_at = $2 WHERE state = $3 AND updated_at < $4`, SourceWorkStatePending, now, SourceWorkStateProcessing, now.Add(-staleAfter)); err != nil {
				return err
			}
		}
		row := tx.QueryRowContext(ctx, `SELECT id, kind, state, processor_id, idempotency_key, service_id, spec_revision, provider, provider_repository_external_id, provider_scope_external_id, tracked_ref, commit_sha, commit_message, commit_author, last_error, attempt_count, available_at, created_at, updated_at FROM source_work_items WHERE state = $1 AND available_at <= $2 ORDER BY available_at ASC, created_at ASC, id ASC LIMIT 1`, SourceWorkStatePending, now)
		if err := row.Scan(&rec.ID, &rec.Kind, &rec.State, &rec.ProcessorID, &rec.IdempotencyKey, &rec.ServiceID, &rec.SpecRevision, &rec.Provider, &rec.ProviderRepositoryExternalID, &rec.ProviderScopeExternalID, &rec.TrackedRef, &rec.CommitSHA, &rec.CommitMessage, &rec.CommitAuthor, &rec.LastError, &rec.AttemptCount, &rec.AvailableAt, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec.State, rec.ProcessorID, rec.UpdatedAt = SourceWorkStateProcessing, processorID, now
		_, err = tx.ExecContext(ctx, `UPDATE source_work_items SET state = $1, processor_id = $2, updated_at = $3 WHERE id = $4`, rec.State, rec.ProcessorID, rec.UpdatedAt, rec.ID)
		return err
	})
	return rec, err
}

func (s *SQLStore) CompleteSourceWorkItem(ctx context.Context, id, processorID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM source_work_items WHERE id = $1 AND state = $2 AND processor_id = $3`, id, SourceWorkStateProcessing, processorID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: source work item %s", ErrLeaseLost, id)
	}
	return nil
}

func (s *SQLStore) ReleaseSourceWorkItem(ctx context.Context, id, processorID string, processErr error, retryAfter time.Duration) error {
	message := ""
	if processErr != nil {
		message = processErr.Error()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE source_work_items SET state = $1, processor_id = '', last_error = $2, attempt_count = attempt_count + 1, available_at = statement_timestamp() + $3::INT8 * INTERVAL '1 microsecond', updated_at = statement_timestamp() WHERE id = $4 AND state = $5 AND processor_id = $6`, SourceWorkStatePending, message, retryAfter.Microseconds(), id, SourceWorkStateProcessing, processorID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: source work item %s", ErrLeaseLost, id)
	}
	return nil
}

func (s *SQLStore) RecoverSourceWorkItems(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE source_work_items SET state = $1, processor_id = '', updated_at = statement_timestamp() WHERE state = $3 AND updated_at < statement_timestamp() - $2::INT8 * INTERVAL '1 microsecond'`, SourceWorkStatePending, staleAfter.Microseconds(), SourceWorkStateProcessing)
		return err
	})
}

func (s *SQLStore) EnqueueSourceWorkItem(ctx context.Context, rec SourceWorkItemRecord) (bool, error) {
	inserted := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		inserted, err = s.EnqueueSourceWorkItemTx(ctx, tx, rec)
		return err
	})
	return inserted, err
}

func (s *SQLStore) EnqueueSourceWorkItemTx(ctx context.Context, tx *sql.Tx, rec SourceWorkItemRecord) (bool, error) {
	now, err := dbtx.DatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	if rec.AvailableAt.IsZero() {
		rec.AvailableAt = now
	}
	rec.State = SourceWorkStatePending
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
