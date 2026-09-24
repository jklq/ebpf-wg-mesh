package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

// DeletionGCStats reports one garbage-collection pass.
type DeletionGCStats struct {
	Collected int
	ByKind    map[string]int
}

// DeletionGC removes expired tombstones, committing each removal separately.
type DeletionGC struct {
	store     *persistence
	notifier  deliverycore.PlatformNotifier
	ingress   deliverycore.PlatformIngress
	interval  time.Duration
	batchSize int
	failOn    func(kind, id string) error
	purgeLogs func(ctx context.Context, projectID string) error
}

// NewDeletionGC builds the collector. Non-positive intervals and batch sizes
// select defaults.
func NewDeletionGC(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, interval time.Duration) *DeletionGC {
	if interval <= 0 {
		interval = time.Minute
	}
	return &DeletionGC{store: store, notifier: notifier, ingress: ingress, interval: interval, batchSize: 100}
}

// SetFailHook installs a test-only fault injector consulted before each
// collection. A nil hook disables injection.
func (g *DeletionGC) SetFailHook(hook func(kind, id string) error) {
	g.failOn = hook
}

// SetLogPurgeHook installs the post-commit log purge for destroyed
// projects. Purge failures are best-effort: per-row TTL expiry
// remains the backstop.
func (g *DeletionGC) SetLogPurgeHook(hook func(ctx context.Context, projectID string) error) {
	g.purgeLogs = hook
}

// Run collects expired tombstones every interval until ctx ends.
func (g *DeletionGC) Run(ctx context.Context) error {
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
func (g *DeletionGC) CollectOnce(ctx context.Context, cutoff time.Time) (DeletionGCStats, error) {
	stats := DeletionGCStats{ByKind: make(map[string]int)}
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
				if item.Kind == ExpiredDeletionProject && g.purgeLogs != nil {
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
