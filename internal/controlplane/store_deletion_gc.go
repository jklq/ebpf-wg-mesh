package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
)

// Expired-deletion kinds collected by garbage collection.
const (
	ExpiredDeletionProject     = "project"
	ExpiredDeletionEnvironment = "environment"
	ExpiredDeletionService     = "service"
	ExpiredDeletionVolume      = "volume"
	ExpiredDeletionDomain      = "domain"
)

// ExpiredDeletion is one tombstone whose grace period ended.
type ExpiredDeletion struct {
	Kind string
	ID   string
}

// listExpiredDeletions returns up to limit tombstones per resource kind whose
// grace period ended at or before cutoff, oldest first.
func (p *persistence) listExpiredDeletions(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredDeletion, error) {
	if limit <= 0 {
		limit = 100
	}
	queries := []struct {
		kind  string
		query string
	}{
		{ExpiredDeletionProject, `SELECT id FROM projects WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionEnvironment, `SELECT id FROM environments WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionService, `SELECT id FROM services WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionVolume, `SELECT id FROM volumes WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionDomain, `SELECT hostname FROM domain_bindings WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, hostname ASC LIMIT $2`},
	}
	var out []ExpiredDeletion
	for _, item := range queries {
		rows, err := p.db.QueryContext(ctx, item.query, cutoff, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ExpiredDeletion{Kind: item.kind, ID: id})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hardDeleteExpired irreversibly deletes one expired tombstone. It reports
// whether a row was destroyed; a restored or concurrently collected row is a
// no-op success, so retries and concurrent collectors stay idempotent.
func (p *persistence) hardDeleteExpired(ctx context.Context, deletion ExpiredDeletion, cutoff time.Time) (bool, error) {
	switch deletion.Kind {
	case ExpiredDeletionProject:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM projects WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx,
					`service_id IN (SELECT id FROM services WHERE environment_id IN (SELECT id FROM environments WHERE project_id = $1))`,
					id); err != nil {
					return err
				}
				return journal.RecordProjectRemoval(ctx, tx, id)
			},
			`DELETE FROM projects WHERE id = $1`)
	case ExpiredDeletionEnvironment:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM environments WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx,
					`service_id IN (SELECT id FROM services WHERE environment_id = $1)`,
					id); err != nil {
					return err
				}
				return journal.RecordEnvironmentRemoval(ctx, tx, id)
			},
			`DELETE FROM environments WHERE id = $1`)
	case ExpiredDeletionService:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM services WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx, `service_id = $1`, id); err != nil {
					return err
				}
				return journal.RecordServiceRemoval(ctx, tx, id)
			},
			`DELETE FROM services WHERE id = $1`)
	case ExpiredDeletionVolume:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM volumes WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error { journal.RecordVolume(ctx, id); return nil },
			`DELETE FROM volumes WHERE id = $1`)
	case ExpiredDeletionDomain:
		return hardDeleteExpiredDomain(ctx, p.database, deletion.ID, cutoff)
	default:
		return false, nil
	}
}

// detachArtifactReferencesTx clears artifact FKs that RESTRICT deletion of
// build_artifacts. Service/environment/project collection cascades into
// artifacts in an unspecified order; leaving these pointers in place can
// block the delete. Prune still uses RESTRICT so live rollback material
// cannot be removed while these rows exist.
func detachArtifactReferencesTx(ctx context.Context, tx *sql.Tx, servicePredicate string, args ...any) error {
	statements := []string{
		`UPDATE service_delivery_status SET current_artifact_id = NULL WHERE ` + servicePredicate,
		`UPDATE service_rollouts SET artifact_id = NULL WHERE ` + servicePredicate,
		`UPDATE deployment_transitions SET artifact_id = NULL WHERE deployment_id IN (SELECT id FROM deployments WHERE ` + servicePredicate + `)`,
		`UPDATE deployments SET artifact_id = NULL WHERE ` + servicePredicate,
	}
	for _, q := range statements {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

func hardDeleteExpiredRow(ctx context.Context, db *database, id string, cutoff time.Time, lockQuery string, record func(context.Context, *sql.Tx, string) error, deleteQuery string) (bool, error) {
	var collected bool
	err := db.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var found string
		if err := tx.QueryRowContext(ctx, lockQuery, id, cutoff).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := record(ctx, tx, found); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, deleteQuery, found); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}

func hardDeleteExpiredDomain(ctx context.Context, db *database, hostname string, cutoff time.Time) (bool, error) {
	var collected bool
	err := db.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var serviceID string
		err := tx.QueryRowContext(ctx,
			`SELECT service_id FROM domain_bindings WHERE hostname = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			hostname, cutoff,
		).Scan(&serviceID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		journal.RecordDomain(ctx, hostname, serviceID)
		if _, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE hostname = $1`, hostname); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}
