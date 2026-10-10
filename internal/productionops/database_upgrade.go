package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

func (r *Runner) upgradePlacement(args []string) (deploy.Placement, error) {
	if len(args) != 2 || r.Plan.Previous == nil {
		return deploy.Placement{}, fmt.Errorf("database-upgrade requires an existing node instance")
	}
	for _, pl := range r.Plan.Previous.Placements {
		if pl.Role == deploy.Database && pl.Instance == args[1] && slices.Contains(r.Plan.Placements, pl) {
			return pl, nil
		}
	}
	return deploy.Placement{}, fmt.Errorf("database upgrade target is not a retained cluster member")
}

func (r *Runner) upgradeInventory(ctx context.Context, convergence bool) (deploy.DatabaseStatus, error) {
	plan := r.Plan
	plan.Placements = r.Plan.Previous.Placements
	return r.inspectDatabase(ctx, plan, convergence)
}

// Check health immediately before the restart, including on retries. Recovery
// of this same stopped node may proceed while it is unavailable; another dead
// member or a range without a surviving voting quorum still blocks admission.
func (r *Runner) prepareDatabaseUpgrade(ctx context.Context, args []string) error {
	pl, err := r.upgradePlacement(args)
	if err != nil {
		return err
	}
	h, _ := r.Plan.Installation.Host(pl.Host)
	address := net.JoinHostPort(h.Network.Address, "26257")
	status, err := r.upgradeInventory(ctx, false)
	if err != nil {
		return err
	}
	running, err := r.remote(ctx, r.Plan, pl, "if systemctl is-active --quiet "+shell(unit(r.Plan, pl))+"; then printf running; else printf stopped; fi\n")
	if err != nil {
		return err
	}
	recovering := string(running) == "stopped"
	if err := databaseRestartHealth(status, pl.Host, recovering); err != nil {
		return err
	}
	for _, version := range status.Versions {
		observed := strings.TrimPrefix(version, "v")
		if observed != strings.TrimPrefix(r.Plan.Previous.Release.Dependencies["cockroachdb"], "v") && observed != strings.TrimPrefix(r.Plan.Release.Dependencies["cockroachdb"], "v") {
			return fmt.Errorf("cluster contains a node outside the pinned upgrade versions")
		}
	}
	if recovering {
		// Only the journaled current replica can recover without full health.
		var state deploy.State
		if err := privateJSON(os.Getenv("PLATFORM_DEPLOYMENT_STATE"), &state); err != nil {
			return err
		}
		if state.Progress == nil || !state.Progress.Started[os.Getenv("PLATFORM_OPERATION")] {
			return fmt.Errorf("stopped database node has no journaled upgrade")
		}
		return nil
	}
	if err := r.verifySurvivingSQLClients(ctx, address); err != nil {
		return err
	}
	if cockroachMajor(r.Plan.Previous.Release.Dependencies["cockroachdb"]) != cockroachMajor(r.Plan.Release.Dependencies["cockroachdb"]) {
		db, err := r.db(ctx, false)
		if err != nil {
			return err
		}
		defer db.Close()
		var version string
		if err := db.QueryRowContext(ctx, "SHOW CLUSTER SETTING version").Scan(&version); err != nil {
			return err
		}
		if err := validateUpgradeClusterVersion(version, status, r.Plan.Previous.Release.Dependencies["cockroachdb"], r.Plan.Release.Dependencies["cockroachdb"]); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "SET CLUSTER SETTING cluster.auto_upgrade.enabled = false"); err != nil {
			return err
		}
	}
	return nil // systemd SIGTERM performs CockroachDB's native drain.
}

func validateUpgradeClusterVersion(version string, status deploy.DatabaseStatus, from, to string) error {
	if version == cockroachMajor(from) {
		return nil
	}
	if version == cockroachMajor(to) && len(status.Versions) == len(status.Members) {
		// A retry after finalization may repair a desired-version node. Never
		// admit an old binary into an already finalized newer cluster.
		for _, build := range status.Versions {
			if strings.TrimPrefix(build, "v") != strings.TrimPrefix(to, "v") {
				return fmt.Errorf("finalized CockroachDB cluster contains an old binary")
			}
		}
		return nil
	}
	return fmt.Errorf("previous CockroachDB major version has not finalized")
}

func databaseRestartHealth(status deploy.DatabaseStatus, target string, recovering bool) error {
	if len(status.Members) < 3 || !slices.Contains(status.Members, target) || len(status.Ranges) == 0 {
		return fmt.Errorf("rolling database restart requires at least three observed members and voting ranges; expand the cluster first")
	}
	if !recovering && (!status.Replicated || !slices.Contains(status.Live, target)) {
		return fmt.Errorf("database restart waits for full replication health")
	}
	for _, member := range status.Members {
		if member != target && !slices.Contains(status.Live, member) {
			return fmt.Errorf("database member %s is unavailable; another restart is unsafe", member)
		}
	}
	for _, row := range status.Ranges {
		live := 0
		for _, voter := range row.Voters {
			if voter != target && slices.Contains(status.Live, voter) {
				live++
			}
		}
		if live < len(row.Voters)/2+1 {
			return fmt.Errorf("range %s cannot retain a voting quorum while %s restarts", row.ID, target)
		}
	}
	return nil
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
	selectDatabaseHost(u, net.JoinHostPort(h.Network.Address, "26257"))
	if err := probeSQL(ctx, u.String()); err != nil {
		return deploy.DatabaseStatus{}, fmt.Errorf("upgraded node is not accepting SQL: %w", err)
	}
	status, err := r.upgradeInventory(ctx, true)
	if err != nil {
		return status, err
	}
	if strings.TrimPrefix(status.Versions[pl.Host], "v") != strings.TrimPrefix(r.Plan.Release.Dependencies["cockroachdb"], "v") {
		return status, fmt.Errorf("node %s has not rejoined with the pinned CockroachDB version", pl.Host)
	}
	return status, nil
}

func cockroachMajor(version string) string {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:2], ".")
}

func (r *Runner) finalizeDatabaseUpgrade(ctx context.Context, verify bool) error {
	if r.Plan.Previous == nil {
		return fmt.Errorf("database finalization requires an upgrade")
	}
	status, err := r.databaseStatus(ctx)
	if err != nil {
		return err
	}
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Database && strings.TrimPrefix(status.Versions[pl.Host], "v") != strings.TrimPrefix(r.Plan.Release.Dependencies["cockroachdb"], "v") {
			return fmt.Errorf("database finalization waits for every node's pinned version")
		}
	}
	previous, desired := cockroachMajor(r.Plan.Previous.Release.Dependencies["cockroachdb"]), cockroachMajor(r.Plan.Release.Dependencies["cockroachdb"])
	if previous == desired {
		return nil
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var version string
	if err := db.QueryRowContext(ctx, "SHOW CLUSTER SETTING version").Scan(&version); err != nil {
		return err
	}
	if version == desired {
		return nil
	}
	if verify {
		return fmt.Errorf("CockroachDB major-version finalization is pending")
	}
	// Finalize explicitly after all nodes pass readiness and replication gates.
	_, err = db.ExecContext(ctx, "SET CLUSTER SETTING version = '"+desired+"'")
	return err
}

func (r *Runner) verifySurvivingSQLClients(ctx context.Context, excluded string) error {
	for _, pl := range r.Plan.Previous.Placements {
		if pl.Role != deploy.ControlPlane && pl.Role != deploy.Console {
			continue
		}
		// Inspect the actual running environment as well as credentials. A
		// rewritten file cannot establish failover for an older running process.
		script := "pid=$(systemctl show --value -p MainPID " + shell(unit(r.Plan, pl)) + ")\ntest \"$pid\" -gt 1\n"
		script += shell(operationsBinary(r.Plan)) + " sql-failover " + shell(string(pl.Role)) + " " + shell(excluded) + " < /proc/$pid/environ\n"
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return fmt.Errorf("client %s cannot reach surviving database nodes: %w", pl.Instance, err)
		}
	}
	return nil
}

func probeSQL(ctx context.Context, address string) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", address)
	if err != nil {
		return err
	}
	defer db.Close()
	var one int
	return db.QueryRowContext(probe, "SELECT 1").Scan(&one)
}
