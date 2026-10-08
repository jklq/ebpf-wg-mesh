package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"github.com/google/uuid"
)

// Automatic reconciliation refreshes the actual online agents it configures.
// Retained workers have no install operation and keep their existing authority.
func (r *Runner) managedCredentials(pl deploy.Placement) bool {
	if !r.Plan.Automatic {
		return true
	}
	if pl.Role == deploy.Builder {
		return false
	}
	if pl.Role != deploy.Agent {
		return true
	}
	for _, op := range r.Plan.Operations {
		if op.Kind == "install" && op.Placement != nil && op.Placement.Instance == pl.Instance {
			return true
		}
	}
	return false
}

// Client issuance caches and native database client credentials must survive
// loss of the first completer. Copy them after provisioning, not before it.
func (r *Runner) replicateCredentialInputs(ctx context.Context) error {
	files := map[string][]byte{}
	links := map[string]string{}
	for _, root := range []string{filepath.Join(r.Config.StateDirectory, r.Plan.Generation), r.databasePKI()} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if filepath.IsAbs(target) || filepath.Clean(target) != target || target == ".." || strings.HasPrefix(target, "../") {
					return fmt.Errorf("credential link escapes its private generation")
				}
				links[path] = target
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("credential cache contains a nonregular input")
			}
			b, err := os.ReadFile(path)
			if err == nil {
				files[path] = b
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	hosts, err := r.completionHosts()
	if err != nil {
		return err
	}
	retained := 0
	for _, host := range hosts {
		var script string
		for _, path := range sortedFiles(files) {
			script += remoteFile(path, files[path]) + verifyRemoteFile(path, files[path])
		}
		for path, target := range links {
			script += "ln -sfn " + shell(target) + " " + shell(path+".next") + "\nmv -Tf " + shell(path+".next") + " " + shell(path) + "\ntest \"$(readlink " + shell(path) + ")\" = " + shell(target) + "\n"
		}
		if _, err := r.remote(ctx, r.Plan, deploy.Placement{Host: host}, script); err == nil {
			retained++
		}
	}
	if retained == 0 {
		return fmt.Errorf("no independent completer retained actual issued credentials")
	}
	return nil
}

// Only one completer may issue credentials at a time. SQL ownership survives
// local interruption; a bounded operation finishes before its durable lease
// expires, and a replacement generation cannot admit the lost authority.
func (r *Runner) acquireRenewal(ctx context.Context, db *sql.DB) (func(), error) {
	return r.acquireNativeLease(ctx, db, "credential_renewal", false, true)
}
func (r *Runner) acquireMaintenance(ctx context.Context, db *sql.DB, allowPaused bool) (func(), error) {
	return r.acquireNativeLease(ctx, db, "credential_renewal", allowPaused, false)
}
func (r *Runner) acquireNativeLease(ctx context.Context, db *sql.DB, kind string, allowPaused, selected bool) (func(), error) {
	if kind != "credential_renewal" && kind != "backup_completion" {
		return nil, fmt.Errorf("unknown native maintenance lease")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.maintenance_plan(installation STRING PRIMARY KEY,generation STRING NOT NULL,plan_id STRING NOT NULL)`); err != nil {
		return nil, err
	}
	table := "platform_recovery.public." + kind
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+table+`(installation STRING PRIMARY KEY,generation STRING NOT NULL,owner STRING NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`); err != nil {
		return nil, err
	}
	owner := uuid.NewString()
	result, err := db.ExecContext(ctx, "INSERT INTO "+table+` SELECT $1,$2,$3,statement_timestamp()+INTERVAL '15 minutes' FROM recovery_runtime_authority WHERE singleton=TRUE AND installation=$1 AND generation=$2 AND (paused=FALSE OR $4) AND (NOT $5 OR EXISTS(SELECT 1 FROM platform_recovery.public.maintenance_plan WHERE installation=$1 AND generation=$2 AND plan_id=$6)) ON CONFLICT(installation) DO UPDATE SET generation=excluded.generation,owner=excluded.owner,expires_at=excluded.expires_at WHERE `+kind+`.expires_at<=statement_timestamp() OR `+kind+`.generation<>excluded.generation`, r.Plan.Installation.ID, r.Plan.Generation, owner, allowPaused, selected, r.Plan.ID)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, fmt.Errorf("%s waits for its applied plan, authority and current owner", kind)
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		db.ExecContext(cleanup, "DELETE FROM "+table+` WHERE installation=$1 AND owner=$2`, r.Plan.Installation.ID, owner)
	}, nil
}

// Hold both kinds of maintenance while replacing their admitted plan. A
// disconnected old completer cannot publish or renew using obsolete placement
// inputs after these durable owners yield or expire.
func (r *Runner) admitMaintenancePlan(ctx context.Context, db *sql.DB) (func(), error) {
	credentials, err := r.acquireMaintenance(ctx, db, true)
	if err != nil {
		return nil, err
	}
	backup, err := r.acquireNativeLease(ctx, db, "backup_completion", true, false)
	if err != nil {
		credentials()
		return nil, err
	}
	release := func() { backup(); credentials() }
	result, err := db.ExecContext(ctx, `UPSERT INTO platform_recovery.public.maintenance_plan SELECT $1,$2,$3 FROM recovery_runtime_authority WHERE singleton=TRUE AND installation=$1 AND generation=$2`, r.Plan.Installation.ID, r.Plan.Generation, r.Plan.ID)
	if err != nil {
		release()
		return nil, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		release()
		return nil, fmt.Errorf("schedule replacement has no admitted runtime authority")
	}
	return release, nil
}

func (r *Runner) verifyMaintenancePlan(ctx context.Context, db *sql.DB) error {
	var generation, plan string
	if err := db.QueryRowContext(ctx, `SELECT generation,plan_id FROM platform_recovery.public.maintenance_plan WHERE installation=$1`, r.Plan.Installation.ID).Scan(&generation, &plan); err != nil {
		return err
	}
	if generation != r.Plan.Generation || plan != r.Plan.ID {
		return fmt.Errorf("independent maintenance belongs to another applied plan")
	}
	return nil
}

func (r *Runner) credentialTimers(ctx context.Context, active bool) error {
	// Include old completers before an upgrade so no independently running
	// renewal can overwrite pause admission after quiescence has begun.
	plans := []deploy.Plan{r.Plan}
	if !active && r.Plan.Previous != nil {
		old := r.Plan.Previous
		plans = append(plans, deploy.Plan{Installation: old.Installation, Release: old.Release, Placements: old.Placements, Previous: old, Generation: r.Plan.Generation})
	}
	seen := map[string]bool{}
	for _, p := range plans {
		targets := append([]string{}, r.Config.CompletionHosts...)
		for _, pl := range p.Placements {
			if pl.Role == deploy.ControlPlane {
				targets = append(targets, pl.Host)
			}
		}
		for _, host := range targets {
			if seen[host] {
				continue
			}
			if _, ok := p.Installation.Host(host); !ok {
				continue
			}
			seen[host] = true
			pl := deploy.Placement{Host: host}
			name := "platform-" + p.Installation.ID + "-recovery-credentials"
			script := ""
			for _, suffix := range []string{".timer", ".service"} {
				if active && suffix == ".service" {
					continue
				}
				action := "stop"
				if active {
					action = "start"
				}
				script += "if systemctl cat " + shell(name+suffix) + " >/dev/null 2>&1; then systemctl " + action + " " + shell(name+suffix) + "; fi\n"
			}
			if _, err := r.remote(ctx, p, pl, script); err != nil {
				if !r.Plan.Recovery || r.verifyHostFenced(ctx, p, pl.Host) != nil {
					return err
				}
			}
		}
	}
	return nil
}
