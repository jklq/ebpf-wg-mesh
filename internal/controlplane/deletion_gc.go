package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

type deletionGCStats struct {
	Collected int
	ByKind    map[string]int
}

type deletionGC struct {
	store     *persistence
	notifier  deliverycore.PlatformNotifier
	ingress   deliverycore.PlatformIngress
	interval  time.Duration
	batchSize int
	failOn    func(kind, id string) error
	purgeLogs func(ctx context.Context, projectID string) error
}

// newDeletionGC builds the collector. Non-positive intervals and batch sizes
// select defaults.
func newDeletionGC(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, interval time.Duration) *deletionGC {
	if interval <= 0 {
		interval = time.Minute
	}
	return &deletionGC{store: store, notifier: notifier, ingress: ingress, interval: interval, batchSize: 100}
}

// SetFailHook installs a test-only fault injector consulted before each
// collection. A nil hook disables injection.
func (g *deletionGC) SetFailHook(hook func(kind, id string) error) {
	g.failOn = hook
}

// SetLogPurgeHook installs the post-commit log purge for destroyed
// projects. Purge failures are best-effort: per-row TTL expiry
// remains the backstop.
func (g *deletionGC) SetLogPurgeHook(hook func(ctx context.Context, projectID string) error) {
	g.purgeLogs = hook
}

// Run collects expired tombstones every interval until ctx ends.
func (g *deletionGC) Run(ctx context.Context) error {
	collect := func() {
		cutoff, err := dbtx.DatabaseTime(ctx, g.store.db)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("deletion gc database time failed", "error", err)
			}
			return
		}
		stats, err := g.CollectOnce(ctx, cutoff)
		if err != nil && ctx.Err() == nil {
			slog.Warn("deletion gc pass had failures", "error", err)
		}
		if stats.Collected > 0 {
			slog.Info("deletion gc pass completed", "collected", stats.Collected, "by_kind", stats.ByKind)
		}
	}
	collect()
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			collect()
		}
	}
}

// CollectOnce destroys every tombstone expired at or before cutoff. Each
// tombstone commits independently; failures are aggregated and the rest of
// the pass continues, so a later pass retries exactly the leftovers.
func (g *deletionGC) CollectOnce(ctx context.Context, cutoff time.Time) (deletionGCStats, error) {
	stats := deletionGCStats{ByKind: make(map[string]int)}
	var errs []error
	for range 100 {
		expired, err := g.store.listExpiredDeletions(ctx, cutoff, g.batchSize)
		if err != nil {
			return stats, err
		}
		if len(expired) == 0 {
			break
		}
		collectedThisPass := 0
		for _, item := range expired {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if g.failOn != nil {
				if err := g.failOn(item.Kind, item.ID); err != nil {
					errs = append(errs, fmt.Errorf("collect %s %s: %w", item.Kind, item.ID, err))
					continue
				}
			}
			collected, err := g.store.hardDeleteExpired(ctx, item, cutoff)
			if err != nil {
				errs = append(errs, fmt.Errorf("collect %s %s: %w", item.Kind, item.ID, err))
				continue
			}
			if collected {
				stats.Collected++
				collectedThisPass++
				stats.ByKind[item.Kind]++
				if item.Kind == expiredDeletionProject && g.purgeLogs != nil {
					if err := g.purgeLogs(ctx, item.ID); err != nil && ctx.Err() == nil {
						slog.Warn("project log purge failed; TTL expiry remains the backstop",
							"project_id", item.ID, "error", err)
					}
				}
			}
		}
		if len(expired) < g.batchSize || collectedThisPass == 0 {
			break
		}
	}
	if stats.Collected > 0 {
		if ids, err := g.store.reads.AgentIDs(ctx); err != nil {
			errs = append(errs, fmt.Errorf("collect notify agents: %w", err))
		} else if g.notifier != nil {
			for _, id := range ids {
				g.notifier.Notify(id)
			}
		}
		if g.ingress != nil {
			g.ingress.RequestSync()
		}
	}
	return stats, errors.Join(errs...)
}

// Expired-deletion kinds collected by garbage collection.
const (
	expiredDeletionProject     = "project"
	expiredDeletionEnvironment = "environment"
	expiredDeletionService     = "service"
	expiredDeletionVolume      = "volume"
	expiredDeletionDomain      = "domain"
)

// ExpiredDeletion is one tombstone whose grace period ended.
type expiredDeletion struct {
	Kind string
	ID   string
}

// listExpiredDeletions returns up to limit tombstones per resource kind whose
// grace period ended at or before cutoff, oldest first.
func (p *persistence) listExpiredDeletions(ctx context.Context, cutoff time.Time, limit int) ([]expiredDeletion, error) {
	if limit <= 0 {
		limit = 100
	}
	queries := []struct {
		kind  string
		query string
	}{
		{expiredDeletionProject, `SELECT id FROM projects WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{expiredDeletionEnvironment, `SELECT id FROM environments WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{expiredDeletionService, `SELECT id FROM services WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{expiredDeletionVolume, `SELECT id FROM volumes WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{expiredDeletionDomain, `SELECT hostname FROM domain_bindings WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, hostname ASC LIMIT $2`},
	}
	var out []expiredDeletion
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
			out = append(out, expiredDeletion{Kind: item.kind, ID: id})
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
func (p *persistence) hardDeleteExpired(ctx context.Context, deletion expiredDeletion, cutoff time.Time) (bool, error) {
	switch deletion.Kind {
	case expiredDeletionProject:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff, journal.ProjectTree(deletion.ID),
			`SELECT id FROM projects WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx,
					`service_id IN (SELECT id FROM services WHERE environment_id IN (SELECT id FROM environments WHERE project_id = $1))`,
					id); err != nil {
					return err
				}
				return deliverycore.RequestVolumeDestructionsTx(ctx, tx, cutoff,
					deliverycore.DeletionTarget{Kind: deliverycore.DeleteProject, ID: id})
			},
			`DELETE FROM projects WHERE id = $1`)
	case expiredDeletionEnvironment:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff, journal.EnvironmentTree(deletion.ID),
			`SELECT id FROM environments WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx,
					`service_id IN (SELECT id FROM services WHERE environment_id = $1)`,
					id); err != nil {
					return err
				}
				return deliverycore.RequestVolumeDestructionsTx(ctx, tx, cutoff, deliverycore.DeletionTarget{Kind: deliverycore.DeleteEnvironment, ID: id})
			},
			`DELETE FROM environments WHERE id = $1`)
	case expiredDeletionService:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff, journal.ServiceTree(deletion.ID),
			`SELECT id FROM services WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				if err := detachArtifactReferencesTx(ctx, tx, `service_id = $1`, id); err != nil {
					return err
				}
				return nil
			},
			`DELETE FROM services WHERE id = $1`)
	case expiredDeletionVolume:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff, journal.VolumeRow(deletion.ID),
			`SELECT id FROM volumes WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				return deliverycore.RequestVolumeDestructionsTx(ctx, tx, cutoff, deliverycore.DeletionTarget{Kind: deliverycore.DeleteVolume, ID: id})
			},
			`DELETE FROM volumes WHERE id = $1`)
	case expiredDeletionDomain:
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

func hardDeleteExpiredRow(ctx context.Context, db *database, id string, cutoff time.Time, effect journal.Mutation, lockQuery string, beforeDelete func(context.Context, *sql.Tx, string) error, deleteQuery string) (bool, error) {
	var collected bool
	err := db.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var found string
		if err := tx.QueryRowContext(ctx, lockQuery, id, cutoff).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if beforeDelete != nil {
			if err := beforeDelete(ctx, tx, found); err != nil {
				return err
			}
		}
		if _, err := effect.Exec(ctx, tx, deleteQuery, found); err != nil {
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
		if _, err := journal.DomainRow(hostname).Exec(ctx, tx, `DELETE FROM domain_bindings WHERE hostname = $1`, hostname); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}
