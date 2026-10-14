package productionops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

const registryGCDrainKey = "registry-gc"

func ensureRegistryGCAdmission(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS recovery.registry_gc_admission
		(installation TEXT PRIMARY KEY, plan_id TEXT NOT NULL)`)
	return err
}

// Distribution's offline collector must never overlap a registry writer. Drain
// builds first, then stop and inhibit every registry replica before starting GC.
// The start gates and drain records survive an interrupted caller; the next run
// resumes the same operation. Maintenance leases exclude upgrades and backups.
func (r *Runner) collectRegistry(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	release, err := r.acquireNativeLease(ctx, db, "credential_renewal", false, true)
	if err != nil {
		return err
	}
	defer release()
	releaseBackup, err := r.acquireNativeLease(ctx, db, "backup_completion", false, true)
	if err != nil {
		return err
	}
	defer releaseBackup()
	if err := ensureRegistryGCAdmission(ctx, db); err != nil {
		return err
	}
	var admitted bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recovery.registry_gc_admission WHERE installation=$1 AND plan_id=$2)`, r.Plan.Installation.ID, r.Plan.ID).Scan(&admitted); err != nil {
		return err
	}
	if !admitted {
		return fmt.Errorf("registry collection waits for the applied plan to finish retirement and finalization")
	}
	// Distribution cannot enumerate an empty, never-used repository root.
	// Platform image retention starts after the first reported build artifact.
	var hasImages bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM build_artifacts WHERE kind = 'build')`).Scan(&hasImages); err != nil {
		return err
	}
	if !hasImages {
		return nil
	}
	var registries, builders []deploy.Placement
	for _, pl := range r.Plan.Placements {
		switch pl.Role {
		case deploy.Registry:
			registries = append(registries, pl)
		case deploy.Builder:
			builders = append(builders, pl)
		}
	}
	if len(registries) == 0 {
		return fmt.Errorf("registry collection requires a registry placement")
	}
	// A previous interrupted attempt may have left registry gates in place.
	// Preserve its builder drains until all writers are confirmed available.
	uploadsAvailable := r.verifyRegistryGC(ctx) == nil
	defer func() {
		if !uploadsAvailable {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, pl := range builders {
			err = errors.Join(err, resumeBuilder(cleanup, db, registryGCDrainKey, pl.Instance))
		}
	}()
	for _, pl := range builders {
		if err := drainBuilder(ctx, db, registryGCDrainKey, pl.Instance); err != nil {
			return err
		}
	}
	// Never force an active uploader off its lease to meet the maintenance window.
	for {
		var active int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state = 'running'`).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			break
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return fmt.Errorf("registry collection waits for active uploads: %w", err)
		}
	}
	uploadsAvailable, err = r.collectRegistryOffline(ctx, registries)
	return err
}

// Return availability separately from collection success: failed collection can
// resume uploads, but uncertain collector shutdown must keep them disabled.
func (r *Runner) collectRegistryOffline(ctx context.Context, registries []deploy.Placement) (available bool, err error) {
	available = true
	var stopped []deploy.Placement
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, pl := range stopped {
			if _, stopErr := r.remote(cleanup, r.Plan, pl, r.registryGCStopCollector(pl)); stopErr != nil {
				available = false
				err = errors.Join(err, fmt.Errorf("registry writes remain disabled: stop collector on %s: %w", pl.Host, stopErr))
				return
			}
		}
		for _, pl := range stopped {
			if _, resumeErr := r.remote(cleanup, r.Plan, pl, r.registryGCResumeScript(pl)); resumeErr != nil {
				available = false
				err = errors.Join(err, resumeErr)
				return
			}
		}
	}()
	for _, pl := range registries {
		// Include ambiguous stops in cleanup: the remote effect may have happened.
		stopped = append(stopped, pl)
		if _, err := r.remote(ctx, r.Plan, pl, r.registryGCStopScript(pl)); err != nil {
			return available, err
		}
	}
	// All registry replicas share the selected durable store. Collect it once,
	// with the exact deployed Distribution binary and its storage configuration.
	pl := registries[0]
	_, err = r.remote(ctx, r.Plan, pl, r.registryGCCollectScript(pl))
	return available, err
}

func (r *Runner) registryGCUnit(pl deploy.Placement) string {
	return "platform-" + r.Plan.Installation.ID + "-" + pl.Instance + "-gc.service"
}

func (r *Runner) registryGCMarker(pl deploy.Placement) string {
	return cfgDir(r.Plan, pl) + "/registry-gc-plan"
}

func (r *Runner) registryGCGate(pl deploy.Placement) string {
	return "/etc/systemd/system/" + unit(r.Plan, pl) + ".d/registry-gc.conf"
}

func (r *Runner) registryGCStopScript(pl deploy.Placement) string {
	marker, service := shell(r.registryGCMarker(pl)), shell(unit(r.Plan, pl))
	script := "if test -f " + marker + "; then test \"$(cat " + marker + ")\" = " + shell(r.Plan.ID) + "; else\n"
	script += "systemctl is-active --quiet " + service + "\ntest ! -L " + shell("/run/systemd/system/"+unit(r.Plan, pl)) + "\nfi\n"
	script += remoteFile(r.registryGCMarker(pl), []byte(r.Plan.ID))
	script += remoteFile(r.registryGCGate(pl), []byte("[Unit]\nConditionPathExists=!"+r.registryGCMarker(pl)+"\n"))
	script += "systemctl daemon-reload\nsystemctl stop " + service + "\ntest \"$(systemctl show --value -p ActiveState " + service + ")\" = inactive\ntest \"$(systemctl show --value -p MainPID " + service + ")\" = 0\n"
	return script
}

func (r *Runner) registryGCCollectScript(pl deploy.Placement) string {
	binary := "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/" + pl.Instance + "/program"
	// The gate also fences a delayed remote start after cancellation. Cleanup
	// removes it before restarting writers, so an old queued job cannot run GC.
	collector := "[Unit]\nConditionPathExists=" + r.registryGCGate(pl) + "\n[Service]\nType=oneshot\nTimeoutStartSec=6min\nKillMode=control-group\nExecStart=" + binary + " garbage-collect " + cfgDir(r.Plan, pl) + "/registry.yaml\n"
	return remoteFile("/run/systemd/system/"+r.registryGCUnit(pl), []byte(collector)) + "systemctl daemon-reload\nsystemctl start " + shell(r.registryGCUnit(pl)) + "\n"
}

func (r *Runner) registryGCStopCollector(pl deploy.Placement) string {
	collector := shell(r.registryGCUnit(pl))
	return "if test \"$(systemctl show --value -p LoadState " + collector + ")\" != not-found; then systemctl stop " + collector + "; fi\ncase \"$(systemctl show --value -p ActiveState " + collector + ")\" in inactive|failed) ;; *) exit 1;; esac\n"
}

func (r *Runner) registryGCResumeScript(pl deploy.Placement) string {
	marker, service := shell(r.registryGCMarker(pl)), shell(unit(r.Plan, pl))
	return "if test -f " + marker + "; then\ntest \"$(cat " + marker + ")\" = " + shell(r.Plan.ID) + "\nrm -f " + shell(r.registryGCGate(pl)) + "\nsystemctl daemon-reload\nsystemctl start " + service + "\nsystemctl is-active --quiet " + service + "\nrm " + marker + "\nfi\n"
}

func (r *Runner) verifyRegistryGC(ctx context.Context) error {
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Registry {
			continue
		}
		if _, err := r.remote(ctx, r.Plan, pl, "test ! -e "+shell(r.registryGCMarker(pl))+"\ntest ! -e "+shell(r.registryGCGate(pl))+"\nsystemctl is-active --quiet "+shell(unit(r.Plan, pl))+"\n"); err != nil {
			return err
		}
	}
	return nil
}
