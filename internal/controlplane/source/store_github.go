package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"ebof-wg-mesh/internal/controlplane/dbtx"
)

const (
	githubWebhookStatePending    = "pending"
	githubWebhookStateProcessing = "processing"
	githubWebhookStateProcessed  = "processed"
	githubWebhookStateFailed     = "failed"
)

func (s *SQLStore) UpsertGitHubInstallation(ctx context.Context, rec GitHubInstallationRecord) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return s.upsertGitHubInstallationTx(ctx, tx, rec)
	})
}

func (s *SQLStore) upsertGitHubInstallationTx(ctx context.Context, tx *sql.Tx, rec GitHubInstallationRecord) error {
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	_, err := tx.ExecContext(ctx,
		`INSERT INTO github_installations(
			installation_id, account_login, account_type, target_type, active, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT(installation_id) DO UPDATE
		   SET account_login = excluded.account_login,
		       account_type = excluded.account_type,
		       target_type = excluded.target_type,
		       active = excluded.active,
		       updated_at = excluded.updated_at`,
		rec.InstallationID, rec.AccountLogin, rec.AccountType, rec.TargetType, rec.Active, rec.CreatedAt, rec.UpdatedAt,
	)
	return err
}

func (s *SQLStore) DeactivateGitHubInstallation(ctx context.Context, installationID int64) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE github_installations
			    SET active = FALSE,
			        updated_at = $2
			  WHERE installation_id = $1`,
			installationID, now,
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM github_installation_repositories
			  WHERE installation_id = $1`,
			installationID,
		)
		return err
	})
}

func (s *SQLStore) ReplaceGitHubInstallationRepositories(ctx context.Context, installation GitHubInstallationRecord, repos []GitHubRepositoryRecord) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := s.upsertGitHubInstallationTx(ctx, tx, installation); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM github_installation_repositories
			  WHERE installation_id = $1`,
			installation.InstallationID,
		); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, repo := range repos {
			repo.Owner = strings.ToLower(strings.TrimSpace(repo.Owner))
			repo.Repo = strings.ToLower(strings.TrimSpace(repo.Repo))
			repo.FullName = githubFullName(repo.Owner, repo.Repo)
			if repo.CreatedAt.IsZero() {
				repo.CreatedAt = now
			}
			repo.UpdatedAt = now
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO github_installation_repositories(
					installation_id, repository_id, owner, repo, full_name, private, default_branch, created_at, updated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				installation.InstallationID, repo.RepositoryID, repo.Owner, repo.Repo, repo.FullName, repo.Private, repo.DefaultBranch, repo.CreatedAt, repo.UpdatedAt,
			); err != nil {
				return err
			}
			if err := s.upsertGitHubRepositorySnapshotTx(ctx, tx, GitHubRepositorySnapshotRecord{
				RepositoryID:  repo.RepositoryID,
				Owner:         repo.Owner,
				Repo:          repo.Repo,
				FullName:      repo.FullName,
				Private:       repo.Private,
				DefaultBranch: repo.DefaultBranch,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLStore) GitHubInstallationByID(ctx context.Context, installationID int64) (GitHubInstallationRecord, error) {
	var rec GitHubInstallationRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT installation_id, account_login, account_type, target_type, active, created_at, updated_at
		   FROM github_installations
		  WHERE installation_id = $1`,
		installationID,
	).Scan(&rec.InstallationID, &rec.AccountLogin, &rec.AccountType, &rec.TargetType, &rec.Active, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return GitHubInstallationRecord{}, err
	}
	return rec, nil
}

func (s *SQLStore) GitHubRepositoryGrantByName(ctx context.Context, installationID int64, owner, repo string) (GitHubRepositoryRecord, error) {
	var rec GitHubRepositoryRecord
	fullName := githubFullName(owner, repo)
	err := s.db.QueryRowContext(ctx,
		`SELECT r.installation_id, r.repository_id, r.owner, r.repo, r.full_name, r.private, r.default_branch, r.created_at, r.updated_at
		   FROM github_installation_repositories r
		   JOIN github_installations i ON i.installation_id = r.installation_id
		  WHERE r.installation_id = $1
		    AND r.full_name = $2
		    AND i.active = TRUE`,
		installationID, fullName,
	).Scan(&rec.InstallationID, &rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return GitHubRepositoryRecord{}, err
	}
	return rec, nil
}

func (s *SQLStore) ListGitHubRepositoryGrantsByName(ctx context.Context, owner, repo string) ([]GitHubRepositoryRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.installation_id, r.repository_id, r.owner, r.repo, r.full_name, r.private, r.default_branch, r.created_at, r.updated_at
		   FROM github_installation_repositories r
		   JOIN github_installations i ON i.installation_id = r.installation_id
		  WHERE r.full_name = $1
		    AND i.active = TRUE
		  ORDER BY r.installation_id ASC`,
		githubFullName(owner, repo),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GitHubRepositoryRecord
	for rows.Next() {
		var rec GitHubRepositoryRecord
		if err := rows.Scan(&rec.InstallationID, &rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLStore) LinkProjectGitHubRepository(ctx context.Context, projectID, userID string, view GitHubRepositoryView) error {
	if projectID == "" || userID == "" || view.RepositoryID <= 0 || view.FullName == "" {
		return errors.New("project, user, and repository are required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO project_github_repositories(
			project_id, installation_id, repository_id, full_name, linked_by_user_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (project_id, full_name) DO UPDATE SET
		   installation_id = excluded.installation_id,
		   repository_id = excluded.repository_id,
		   full_name = excluded.full_name,
		   linked_by_user_id = excluded.linked_by_user_id,
		   updated_at = excluded.updated_at`,
		projectID, view.InstallationID, view.RepositoryID, strings.ToLower(view.FullName), userID, now,
	)
	return err
}

func (s *SQLStore) ProjectGitHubRepositoryInstallation(ctx context.Context, projectID, owner, repo string) (int64, error) {
	var installationID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT installation_id
		   FROM project_github_repositories
		  WHERE project_id = $1 AND full_name = $2`,
		projectID, githubFullName(owner, repo),
	).Scan(&installationID)
	return installationID, err
}

func (s *SQLStore) EnqueueGitHubWebhookDelivery(ctx context.Context, deliveryID, eventType string, payload []byte) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO github_webhook_deliveries(
			id, delivery_id, event_type, state, processor_id, payload, last_error, received_at, updated_at, processed_at
		) VALUES ($1, $2, $3, $4, '', $5, '', $6, $6, NULL)
		ON CONFLICT(delivery_id) DO NOTHING`,
		uuid.NewString(), deliveryID, eventType, githubWebhookStatePending, payload, now,
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

func (s *SQLStore) ClaimNextGitHubWebhookDelivery(ctx context.Context, processorID string) (GitHubWebhookDeliveryRecord, error) {
	var rec GitHubWebhookDeliveryRecord
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec = GitHubWebhookDeliveryRecord{}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		// CockroachDB does not support SKIP LOCKED. The row lock plus the
		// guarded UPDATE preserves a single winner under serializable retries.
		row := tx.QueryRowContext(ctx,
			`UPDATE github_webhook_deliveries AS delivery
			SET state = $2, processor_id = $3, updated_at = $4
			WHERE delivery.id = (
				SELECT id FROM github_webhook_deliveries
				WHERE state = $1
				ORDER BY received_at ASC, id ASC
				LIMIT 1 FOR UPDATE
			)
			AND delivery.state = $1
			RETURNING delivery.id, delivery.delivery_id, delivery.event_type, delivery.state,
				delivery.processor_id, delivery.payload, delivery.last_error, delivery.received_at,
				delivery.updated_at, delivery.processed_at`,
			githubWebhookStatePending, githubWebhookStateProcessing, processorID, now,
		)
		if err := row.Scan(&rec.ID, &rec.DeliveryID, &rec.EventType, &rec.State, &rec.ProcessorID, &rec.Payload, &rec.LastError, &rec.ReceivedAt, &rec.UpdatedAt, &rec.ProcessedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return GitHubWebhookDeliveryRecord{}, err
	}
	return rec, nil
}

func (s *SQLStore) CompleteGitHubWebhookDelivery(ctx context.Context, deliveryID, processorID string, processErr error) error {
	state := githubWebhookStateProcessed
	lastError := ""
	if processErr != nil {
		state = githubWebhookStateFailed
		lastError = processErr.Error()
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE github_webhook_deliveries
		    SET state = $1,
		        last_error = $2,
		        processed_at = statement_timestamp(),
		        updated_at = statement_timestamp()
		  WHERE id = $3 AND state = $4 AND processor_id = $5`,
		state, lastError, deliveryID, githubWebhookStateProcessing, processorID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: webhook delivery %s", ErrLeaseLost, deliveryID)
	}
	return nil
}

func (s *SQLStore) UpsertGitHubRepositorySnapshot(ctx context.Context, rec GitHubRepositorySnapshotRecord) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return s.upsertGitHubRepositorySnapshotTx(ctx, tx, rec)
	})
}

func (s *SQLStore) upsertGitHubRepositorySnapshotTx(ctx context.Context, tx *sql.Tx, rec GitHubRepositorySnapshotRecord) error {
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	rec.Owner = strings.ToLower(strings.TrimSpace(rec.Owner))
	rec.Repo = strings.ToLower(strings.TrimSpace(rec.Repo))
	rec.FullName = githubFullName(rec.Owner, rec.Repo)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO github_repository_snapshots(
			full_name, repository_id, owner, repo, private, default_branch, deleted, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT(full_name) DO UPDATE
		   SET repository_id = excluded.repository_id,
		       owner = excluded.owner,
		       repo = excluded.repo,
		       private = excluded.private,
		       default_branch = excluded.default_branch,
		       deleted = excluded.deleted,
		       updated_at = excluded.updated_at`,
		rec.FullName, rec.RepositoryID, rec.Owner, rec.Repo, rec.Private, rec.DefaultBranch, rec.Deleted, rec.CreatedAt, rec.UpdatedAt,
	)
	return err
}

func (s *SQLStore) MarkGitHubRepositorySnapshotDeleted(ctx context.Context, owner, repo string) error {
	fullName := githubFullName(owner, repo)
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE github_repository_snapshots
		    SET deleted = TRUE,
		        updated_at = $3
		  WHERE full_name = $1
		     OR (owner = $2 AND repo = $4)`,
		fullName, owner, now, repo,
	)
	return err
}

func (s *SQLStore) GitHubRepositorySnapshotByName(ctx context.Context, owner, repo string) (GitHubRepositorySnapshotRecord, error) {
	var rec GitHubRepositorySnapshotRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT repository_id, owner, repo, full_name, private, default_branch, deleted, created_at, updated_at
		   FROM github_repository_snapshots
		  WHERE full_name = $1`,
		githubFullName(owner, repo),
	).Scan(&rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.Deleted, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return GitHubRepositorySnapshotRecord{}, err
	}
	return rec, nil
}

func (s *SQLStore) ListActiveGitHubInstallationIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT installation_id
		   FROM github_installations
		  WHERE active = TRUE
		  ORDER BY installation_id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var installationID int64
		if err := rows.Scan(&installationID); err != nil {
			return nil, err
		}
		out = append(out, installationID)
	}
	return out, rows.Err()
}

func (s *SQLStore) RecoverGitHubWebhookDeliveries(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE github_webhook_deliveries
		    SET state = $1,
		        processor_id = '',
		        updated_at = statement_timestamp()
		  WHERE state = $3
		    AND updated_at < statement_timestamp() - $2::INT8 * INTERVAL '1 microsecond'`,
			githubWebhookStatePending,
			staleAfter.Microseconds(),
			githubWebhookStateProcessing,
		)
		return err
	})
}
