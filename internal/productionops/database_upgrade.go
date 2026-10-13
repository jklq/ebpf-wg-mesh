package productionops

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/platformdb"
)

// Database upgrades restart the single primary in place. The plan already
// rejected major-version transitions and primary host moves (both need a
// restore), and the workflow's pre-backup phase took a fresh recovery
// point; these hooks only gate the restart on the observed version.

func (r *Runner) upgradePlacement(args []string) (deploy.Placement, error) {
	if len(args) != 2 || r.Plan.Previous == nil {
		return deploy.Placement{}, fmt.Errorf("database-upgrade requires an existing node instance")
	}
	for _, pl := range r.Plan.Previous.Placements {
		if pl.Role == deploy.Database && pl.Instance == args[1] && slices.Contains(r.Plan.Placements, pl) {
			return pl, nil
		}
	}
	return deploy.Placement{}, fmt.Errorf("database upgrade target is not the retained primary")
}

func (r *Runner) prepareDatabaseUpgrade(ctx context.Context, args []string) error {
	pl, err := r.upgradePlacement(args)
	if err != nil {
		return err
	}
	status, err := r.databaseStatus(ctx)
	if err != nil {
		return err
	}
	running, err := r.remote(ctx, r.Plan, pl, "if systemctl is-active --quiet "+shell(unit(r.Plan, pl))+"; then printf running; else printf stopped; fi\n")
	if err != nil {
		return err
	}
	if string(running) == "stopped" {
		var state deploy.State
		if err := privateJSON(os.Getenv("PLATFORM_DEPLOYMENT_STATE"), &state); err != nil {
			return err
		}
		if state.Progress == nil || !state.Progress.Started[os.Getenv("PLATFORM_OPERATION")] {
			return fmt.Errorf("stopped database primary has no journaled upgrade")
		}
		return nil
	}
	if len(status.Live) != 1 || status.Live[0] != pl.Host {
		return fmt.Errorf("database upgrade waits for the primary on %s to be live", pl.Host)
	}
	observed := platformdb.MajorVersion(status.Versions[pl.Host])
	previous := platformdb.MajorVersion(r.Plan.Previous.Release.Dependencies["postgresql"])
	desired := platformdb.MajorVersion(r.Plan.Release.Dependencies["postgresql"])
	if observed == 0 || (observed != previous && observed != desired) {
		return fmt.Errorf("primary runs PostgreSQL %q outside the pinned upgrade versions", status.Versions[pl.Host])
	}
	return nil // systemd SIGTERM is PostgreSQL's fast shutdown; clients reconnect after the restart.
}

func (r *Runner) verifyDatabaseUpgrade(ctx context.Context, args []string) (deploy.DatabaseStatus, error) {
	pl, err := r.upgradePlacement(args)
	if err != nil {
		return deploy.DatabaseStatus{}, err
	}
	h, _ := r.Plan.Installation.Host(pl.Host)
	data, err := os.ReadFile(r.Config.Database.URLFile)
	if err != nil {
		return deploy.DatabaseStatus{}, err
	}
	u, err := url.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return deploy.DatabaseStatus{}, err
	}
	selectDatabaseHost(u, net.JoinHostPort(h.Network.Address, "5432"))
	if err := probeSQL(ctx, u.String()); err != nil {
		return deploy.DatabaseStatus{}, fmt.Errorf("upgraded primary is not accepting SQL: %w", err)
	}
	status, err := r.databaseStatus(ctx)
	if err != nil {
		return status, err
	}
	desired := platformdb.MajorVersion(r.Plan.Release.Dependencies["postgresql"])
	if platformdb.MajorVersion(status.Versions[pl.Host]) != desired {
		return status, fmt.Errorf("primary on %s has not rejoined with the pinned PostgreSQL major version", pl.Host)
	}
	if !status.Archiving {
		return status, fmt.Errorf("upgraded primary is not archiving WAL")
	}
	return status, nil
}

// finalizeDatabaseUpgrade has no cluster version to finalize for a single
// primary; it verifies the pinned version is live and archiving before
// later phases build on it.
func (r *Runner) finalizeDatabaseUpgrade(ctx context.Context, verify bool) error {
	if r.Plan.Previous == nil {
		return fmt.Errorf("database finalization requires an upgrade")
	}
	status, err := r.databaseStatus(ctx)
	if err != nil {
		return err
	}
	desired := platformdb.MajorVersion(r.Plan.Release.Dependencies["postgresql"])
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Database && platformdb.MajorVersion(status.Versions[pl.Host]) != desired {
			return fmt.Errorf("database finalization waits for the primary's pinned PostgreSQL version")
		}
	}
	if !status.Archiving {
		return fmt.Errorf("database finalization waits for WAL archiving")
	}
	return nil
}

func probeSQL(ctx context.Context, address string) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := platformdb.OpenURL(probe, address, platformdb.SchemaPlatform)
	if err != nil {
		return err
	}
	defer db.Close()
	var one int
	return db.QueryRowContext(probe, "SELECT 1").Scan(&one)
}
