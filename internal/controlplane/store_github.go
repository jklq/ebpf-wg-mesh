package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	githubWebhookStatePending    = "pending"
	githubWebhookStateProcessing = "processing"
	githubWebhookStateProcessed  = "processed"
	githubWebhookStateFailed     = "failed"

	githubWorkKindRefreshInstallation = "refresh_installation"
	githubWorkKindSyncServiceSource   = "sync_service_source"
	githubWorkKindEnqueueBuildCommit  = "enqueue_build_for_commit"

	githubWorkStatePending    = "pending"
	githubWorkStateProcessing = "processing"
)

func (s *Store) upsertGitHubInstallation(ctx context.Context, rec githubInstallationRecord) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.upsertGitHubInstallationTx(ctx, tx, rec)
	})
}

func (s *Store) upsertGitHubInstallationTx(ctx context.Context, tx *sql.Tx, rec githubInstallationRecord) error {
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

func (s *Store) deactivateGitHubInstallation(ctx context.Context, installationID int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
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

func (s *Store) replaceGitHubInstallationRepositories(ctx context.Context, installation githubInstallationRecord, repos []githubRepositoryRecord) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
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
			if err := s.upsertGitHubRepositorySnapshotTx(ctx, tx, githubRepositorySnapshotRecord{
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

func (s *Store) listGitHubInstallations(ctx context.Context) ([]githubInstallationRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT installation_id, account_login, account_type, target_type, active, created_at, updated_at
		   FROM github_installations
		  WHERE active = TRUE
		  ORDER BY account_login ASC, installation_id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []githubInstallationRecord
	for rows.Next() {
		var rec githubInstallationRecord
		if err := rows.Scan(&rec.InstallationID, &rec.AccountLogin, &rec.AccountType, &rec.TargetType, &rec.Active, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) githubInstallationByID(ctx context.Context, installationID int64) (githubInstallationRecord, error) {
	var rec githubInstallationRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT installation_id, account_login, account_type, target_type, active, created_at, updated_at
		   FROM github_installations
		  WHERE installation_id = $1`,
		installationID,
	).Scan(&rec.InstallationID, &rec.AccountLogin, &rec.AccountType, &rec.TargetType, &rec.Active, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return githubInstallationRecord{}, err
	}
	return rec, nil
}

func (s *Store) listGitHubInstallationRepositories(ctx context.Context, installationID int64) ([]githubRepositoryRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.installation_id, r.repository_id, r.owner, r.repo, r.full_name, r.private, r.default_branch, r.created_at, r.updated_at
		   FROM github_installation_repositories r
		   JOIN github_installations i ON i.installation_id = r.installation_id
		  WHERE r.installation_id = $1
		    AND i.active = TRUE
		  ORDER BY r.full_name ASC`,
		installationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []githubRepositoryRecord
	for rows.Next() {
		var rec githubRepositoryRecord
		if err := rows.Scan(&rec.InstallationID, &rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) listGitHubInstallationRepositoryViews(ctx context.Context, installationID int64) ([]GitHubRepositoryView, error) {
	recs, err := s.listGitHubInstallationRepositories(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := make([]GitHubRepositoryView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, GitHubRepositoryView{
			RepositoryID:   rec.RepositoryID,
			Owner:          rec.Owner,
			Repo:           rec.Repo,
			FullName:       rec.FullName,
			Private:        rec.Private,
			DefaultBranch:  rec.DefaultBranch,
			InstallationID: rec.InstallationID,
			AccessState:    sourceAccessStateAvailable,
			GrantUpdatedAt: rec.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Store) githubRepositoryGrantByName(ctx context.Context, installationID int64, owner, repo string) (githubRepositoryRecord, error) {
	var rec githubRepositoryRecord
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
		return githubRepositoryRecord{}, err
	}
	return rec, nil
}

func (s *Store) listGitHubRepositoryGrantsByName(ctx context.Context, owner, repo string) ([]githubRepositoryRecord, error) {
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

	var out []githubRepositoryRecord
	for rows.Next() {
		var rec githubRepositoryRecord
		if err := rows.Scan(&rec.InstallationID, &rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) linkProjectGitHubRepository(ctx context.Context, projectID, userID string, view GitHubRepositoryView) error {
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

func (s *Store) projectGitHubRepositoryInstallation(ctx context.Context, projectID, owner, repo string) (int64, error) {
	var installationID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT installation_id
		   FROM project_github_repositories
		  WHERE project_id = $1 AND full_name = $2`,
		projectID, githubFullName(owner, repo),
	).Scan(&installationID)
	return installationID, err
}

func (s *Store) enqueueGitHubWebhookDelivery(ctx context.Context, deliveryID, eventType string, payload []byte) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO github_webhook_deliveries(
			id, delivery_id, event_type, state, processor_id, payload, last_error, received_at, updated_at, processed_at
		) VALUES ($1, $2, $3, $4, '', $5, '', $6, $6, NULL)
		ON CONFLICT(delivery_id) DO NOTHING`,
		mustID(), deliveryID, eventType, githubWebhookStatePending, payload, now,
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

func (s *Store) claimNextGitHubWebhookDelivery(ctx context.Context, processorID string, staleAfter time.Duration) (githubWebhookDeliveryRecord, error) {
	var rec githubWebhookDeliveryRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if staleAfter > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE github_webhook_deliveries
				    SET state = $1,
				        processor_id = '',
				        updated_at = $2
				  WHERE state = $3
				    AND updated_at < $4`,
				githubWebhookStatePending, now, githubWebhookStateProcessing, now.Add(-staleAfter),
			); err != nil {
				return err
			}
		}
		row := tx.QueryRowContext(ctx,
			`SELECT id, delivery_id, event_type, state, processor_id, payload, last_error, received_at, updated_at, processed_at
			   FROM github_webhook_deliveries
			  WHERE state = $1
			  ORDER BY received_at ASC, id ASC
			  LIMIT 1`,
			githubWebhookStatePending,
		)
		if err := row.Scan(&rec.ID, &rec.DeliveryID, &rec.EventType, &rec.State, &rec.ProcessorID, &rec.Payload, &rec.LastError, &rec.ReceivedAt, &rec.UpdatedAt, &rec.ProcessedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec.State = githubWebhookStateProcessing
		rec.ProcessorID = processorID
		rec.UpdatedAt = now
		_, err := tx.ExecContext(ctx,
			`UPDATE github_webhook_deliveries
			    SET state = $1,
			        processor_id = $2,
			        updated_at = $3
			  WHERE id = $4`,
			rec.State, rec.ProcessorID, rec.UpdatedAt, rec.ID,
		)
		return err
	})
	if err != nil {
		return githubWebhookDeliveryRecord{}, err
	}
	return rec, nil
}

func (s *Store) completeGitHubWebhookDelivery(ctx context.Context, deliveryID string, processErr error) error {
	state := githubWebhookStateProcessed
	lastError := ""
	if processErr != nil {
		state = githubWebhookStateFailed
		lastError = processErr.Error()
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE github_webhook_deliveries
		    SET state = $1,
		        last_error = $2,
		        processed_at = $3,
		        updated_at = $3
		  WHERE id = $4`,
		state, lastError, now, deliveryID,
	)
	return err
}

func (s *Store) enqueueGitHubWorkItem(ctx context.Context, rec githubWorkItemRecord) (bool, error) {
	inserted := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		inserted, err = s.enqueueGitHubWorkItemTx(ctx, tx, rec)
		return err
	})
	return inserted, err
}

func (s *Store) enqueueGitHubWorkItemTx(ctx context.Context, tx *sql.Tx, rec githubWorkItemRecord) (bool, error) {
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = mustID()
	}
	if rec.AvailableAt.IsZero() {
		rec.AvailableAt = now
	}
	rec.State = githubWorkStatePending
	rec.ProcessorID = ""
	rec.LastError = ""
	rec.CreatedAt = now
	rec.UpdatedAt = now
	result, err := tx.ExecContext(ctx,
		`INSERT INTO github_work_items(
			id, kind, state, processor_id, idempotency_key, service_id, spec_revision, installation_id,
			commit_sha, last_error, attempt_count, available_at, created_at, updated_at
		) VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, '', 0, $9, $10, $10)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		rec.ID, rec.Kind, rec.State, rec.IdempotencyKey, rec.ServiceID, rec.SpecRevision, rec.InstallationID, rec.CommitSHA, rec.AvailableAt, rec.CreatedAt,
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

func (s *Store) claimNextGitHubWorkItem(ctx context.Context, processorID string, staleAfter time.Duration) (githubWorkItemRecord, error) {
	var rec githubWorkItemRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if staleAfter > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE github_work_items
				    SET state = $1,
				        processor_id = '',
				        updated_at = $2
				  WHERE state = $3
				    AND updated_at < $4`,
				githubWorkStatePending, now, githubWorkStateProcessing, now.Add(-staleAfter),
			); err != nil {
				return err
			}
		}
		row := tx.QueryRowContext(ctx,
			`SELECT id, kind, state, processor_id, idempotency_key, service_id, spec_revision, installation_id,
			        commit_sha, last_error, attempt_count, available_at, created_at, updated_at
			   FROM github_work_items
			  WHERE state = $1
			    AND available_at <= $2
			  ORDER BY available_at ASC, created_at ASC, id ASC
			  LIMIT 1`,
			githubWorkStatePending, now,
		)
		if err := row.Scan(
			&rec.ID,
			&rec.Kind,
			&rec.State,
			&rec.ProcessorID,
			&rec.IdempotencyKey,
			&rec.ServiceID,
			&rec.SpecRevision,
			&rec.InstallationID,
			&rec.CommitSHA,
			&rec.LastError,
			&rec.AttemptCount,
			&rec.AvailableAt,
			&rec.CreatedAt,
			&rec.UpdatedAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec.State = githubWorkStateProcessing
		rec.ProcessorID = processorID
		rec.UpdatedAt = now
		_, err := tx.ExecContext(ctx,
			`UPDATE github_work_items
			    SET state = $1,
			        processor_id = $2,
			        updated_at = $3
			  WHERE id = $4`,
			rec.State, rec.ProcessorID, rec.UpdatedAt, rec.ID,
		)
		return err
	})
	if err != nil {
		return githubWorkItemRecord{}, err
	}
	return rec, nil
}

func (s *Store) completeGitHubWorkItem(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM github_work_items WHERE id = $1`, id)
	return err
}

func (s *Store) releaseGitHubWorkItem(ctx context.Context, id string, processErr error, retryAfter time.Duration) error {
	now := time.Now().UTC()
	message := ""
	if processErr != nil {
		message = processErr.Error()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE github_work_items
		    SET state = $1,
		        processor_id = '',
		        last_error = $2,
		        attempt_count = attempt_count + 1,
		        available_at = $3,
		        updated_at = $4
		  WHERE id = $5`,
		githubWorkStatePending,
		message,
		now.Add(retryAfter),
		now,
		id,
	)
	return err
}

func (s *Store) recoverGitHubWorkItems(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE github_work_items
		    SET state = $1,
		        processor_id = '',
		        updated_at = $2
		  WHERE state = $3
		    AND updated_at < $4`,
		githubWorkStatePending,
		now,
		githubWorkStateProcessing,
		now.Add(-staleAfter),
	)
	return err
}

func (s *Store) upsertGitHubRepositorySnapshot(ctx context.Context, rec githubRepositorySnapshotRecord) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.upsertGitHubRepositorySnapshotTx(ctx, tx, rec)
	})
}

func (s *Store) upsertGitHubRepositorySnapshotTx(ctx context.Context, tx *sql.Tx, rec githubRepositorySnapshotRecord) error {
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

func (s *Store) markGitHubRepositorySnapshotDeleted(ctx context.Context, owner, repo string) error {
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

func (s *Store) githubRepositorySnapshotByName(ctx context.Context, owner, repo string) (githubRepositorySnapshotRecord, error) {
	var rec githubRepositorySnapshotRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT repository_id, owner, repo, full_name, private, default_branch, deleted, created_at, updated_at
		   FROM github_repository_snapshots
		  WHERE full_name = $1`,
		githubFullName(owner, repo),
	).Scan(&rec.RepositoryID, &rec.Owner, &rec.Repo, &rec.FullName, &rec.Private, &rec.DefaultBranch, &rec.Deleted, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return githubRepositorySnapshotRecord{}, err
	}
	return rec, nil
}

func (s *Store) listActiveGitHubInstallationIDs(ctx context.Context) ([]int64, error) {
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

func (s *Store) recoverGitHubWebhookDeliveries(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE github_webhook_deliveries
		    SET state = $1,
		        processor_id = '',
		        updated_at = $2
		  WHERE state = $3
		    AND updated_at < $4`,
		githubWebhookStatePending,
		now,
		githubWebhookStateProcessing,
		now.Add(-staleAfter),
	)
	return err
}

func githubFullName(owner, repo string) string {
	return strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(repo))
}
