package delivery

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PreviousImageLifetime bounds the one previous successful image per service.
const PreviousImageLifetime = 24 * time.Hour

type ImageDeleter interface {
	PrepareDelete(context.Context, string) (func(context.Context) error, error)
}

func (d *Delivery) SetImageDeleter(deleter ImageDeleter) { d.imageDeleter = deleter }

// PruneBuildArtifacts expires image availability, never historical provenance. The
// database transaction serializes retention against rollout and build decisions.
func (d *Delivery) PruneBuildArtifacts(ctx context.Context, now time.Time) (int64, error) {
	var expired int64
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		expired = 0
		rows, err := tx.QueryContext(ctx, `
			WITH current_images AS (
				SELECT a.service_id, a.image_ref FROM build_artifacts a
				JOIN service_delivery_status s ON s.current_artifact_id = a.id
			), successful_images AS (
				SELECT a.service_id, a.image_ref, MAX(t.occurred_at) AS succeeded_at
				FROM build_artifacts a
				JOIN deployment_transitions t ON t.artifact_id = a.id AND t.to_state = 'active'
				WHERE NOT EXISTS (SELECT 1 FROM current_images c
					WHERE c.service_id = a.service_id AND c.image_ref = a.image_ref)
				GROUP BY a.service_id, a.image_ref
			), previous_images AS (
				SELECT image_ref, succeeded_at,
					row_number() OVER (PARTITION BY service_id ORDER BY succeeded_at DESC, image_ref DESC) AS rank
				FROM successful_images
			), retained_images AS (
				SELECT image_ref FROM current_images
				UNION SELECT image_ref FROM previous_images WHERE rank = 1 AND succeeded_at > $1
				UNION SELECT a.image_ref FROM build_artifacts a JOIN deployments d ON d.artifact_id = a.id
					WHERE d.state IN ('scheduling', 'image_pull', 'starting', 'readiness', 'active', 'draining')
				UNION SELECT a.image_ref FROM build_artifacts a JOIN service_rollouts r ON r.artifact_id = a.id
					WHERE r.state IN ('pending_build', 'in_progress')
				UNION SELECT a.image_ref FROM build_artifacts a JOIN deployments d ON d.artifact_id = a.id
					JOIN allocation_assignments aa ON aa.deployment_id = d.id
			)
			SELECT a.id, a.kind, a.image_ref FROM build_artifacts a
			WHERE a.image_retained = TRUE AND NOT EXISTS (
				SELECT 1 FROM retained_images r WHERE r.image_ref = a.image_ref)
			ORDER BY a.created_at, a.id LIMIT 1000 FOR UPDATE OF a`, now.Add(-PreviousImageLifetime))
		if err != nil {
			return err
		}
		type candidate struct{ id, kind, ref string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.kind, &c.ref); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, c := range candidates {
			if _, err := tx.ExecContext(ctx, `UPDATE build_artifacts SET image_retained = FALSE WHERE id = $1`, c.id); err != nil {
				return err
			}
			if c.kind == BuildArtifactBuild {
				if _, err := tx.ExecContext(ctx, `INSERT INTO registry_image_deletions(image_ref, created_at)
					VALUES ($1, $2) ON CONFLICT DO NOTHING`, c.ref, now); err != nil {
					return err
				}
			}
			expired++
		}
		return nil
	})
	return expired, err
}

// CollectExpiredImages retries durable manifest deletions. Holding the queue row
// lock across the idempotent delete fences concurrent collectors. Registry blob
// collection is the storage operator's job; external direct images are never deleted.
func (d *Delivery) CollectExpiredImages(ctx context.Context) (int, error) {
	if d.imageDeleter == nil {
		return 0, nil
	}
	rows, err := d.store.db.QueryContext(ctx, `SELECT image_ref FROM registry_image_deletions
		ORDER BY created_at, image_ref LIMIT 100`)
	if err != nil {
		return 0, err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, ref)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	collected := 0
	var failures []error
	for _, ref := range refs {
		// Mint auth before taking a database transaction. The signer reads its
		// keys from the same pool, which may have only one open connection.
		deleteImage, err := d.imageDeleter.PrepareDelete(ctx, ref)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		deleted := false
		err = d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			deleted = false
			var locked string
			if err := tx.QueryRowContext(ctx, `SELECT image_ref FROM registry_image_deletions
				WHERE image_ref = $1 FOR UPDATE`, ref).Scan(&locked); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			var retained bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM build_artifacts
				WHERE image_ref = $1 AND image_retained = TRUE)`, ref).Scan(&retained); err != nil {
				return err
			}
			if retained {
				return nil
			}
			if err := deleteImage(ctx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM registry_image_deletions WHERE image_ref = $1`, ref)
			deleted = err == nil
			return err
		})
		if err != nil {
			failures = append(failures, err)
		} else if deleted {
			collected++
		}
	}
	return collected, errors.Join(failures...)
}
