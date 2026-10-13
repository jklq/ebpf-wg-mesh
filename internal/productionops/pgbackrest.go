package productionops

import (
	"context"
	"fmt"
	"strings"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

// pgBackRest runs on the database host as the database service account: it
// archives WAL, takes physical backups, and restores into the data
// directory. Completers only ever read the repository (info) from their
// own rendered copy of the same stanza configuration.

func (r *Runner) databasePlacement() (deploy.Placement, error) {
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Database {
			return pl, nil
		}
	}
	return deploy.Placement{}, fmt.Errorf("plan has no database primary placement")
}

func (r *Runner) pgBackRestCommand(pl deploy.Placement, args ...string) string {
	parts := []string{
		shell(r.databaseTool("pgbackrest")),
		"--config=" + shell(cfgDir(r.Plan, pl)+"/certs/pgbackrest.conf"),
		"--stanza=" + shell(r.Config.Database.PgBackRestStanza),
	}
	for _, arg := range args {
		parts = append(parts, shell(arg))
	}
	return "runuser -u " + shell(deploy.DatabaseOSUser) + " -- " + strings.Join(parts, " ") + "\n"
}

func (r *Runner) remotePgBackRest(ctx context.Context, pl deploy.Placement, args ...string) ([]byte, error) {
	return r.remote(ctx, r.Plan, pl, r.pgBackRestCommand(pl, args...))
}

// ensureStanza creates the stanza once archiving is live and proves the
// whole path with pgBackRest's own check (which archives a test segment).
// On a restored primary the timeline has advanced; stanza-create is the
// supported way to bind the new timeline to the same repository.
func (r *Runner) ensureStanza(ctx context.Context) error {
	pl, err := r.databasePlacement()
	if err != nil {
		return err
	}
	if _, err := r.remotePgBackRest(ctx, pl, "stanza-create"); err != nil {
		return fmt.Errorf("pgBackRest stanza-create: %w", err)
	}
	if _, err := r.remotePgBackRest(ctx, pl, "check"); err != nil {
		return fmt.Errorf("pgBackRest check: %w", err)
	}
	return nil
}

func (r *Runner) backupUnits(pl deploy.Placement) map[string]string {
	name := "platform-" + r.Plan.Installation.ID + "-pgbackrest"
	service := func(kind string) string {
		return "[Unit]\nDescription=pgBackRest " + kind + " backup of the platform database\nAfter=network-online.target\n[Service]\nType=oneshot\nUser=" + deploy.DatabaseOSUser + "\nUMask=0077\nTimeoutStartSec=55min\nExecStart=" +
			strings.TrimSpace(strings.TrimPrefix(r.pgBackRestCommand(pl, "backup", "--type="+kind, "--start-fast"), "runuser -u "+deploy.DatabaseOSUser+" -- ")) + "\n"
	}
	timer := func(calendar string) string {
		return "[Unit]\nDescription=Scheduled pgBackRest backup\n[Timer]\nOnCalendar=" + calendar + "\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	}
	return map[string]string{
		name + "-full.service": service("full"),
		name + "-full.timer":   timer(recovery.FullBackupSchedule),
		name + "-incr.service": service("incr"),
		name + "-incr.timer":   timer(recovery.IncrementalBackupSchedule),
	}
}

// installBackupTimers replaces the removed in-database backup schedules
// with systemd timers on the database host, verified by content like the
// completion timers.
func (r *Runner) installBackupTimers(ctx context.Context, verify bool) error {
	pl, err := r.databasePlacement()
	if err != nil {
		return err
	}
	name := "platform-" + r.Plan.Installation.ID + "-pgbackrest"
	units := r.backupUnits(pl)
	var script strings.Builder
	for _, filename := range sortedFilesString(units) {
		path := "/etc/systemd/system/" + filename
		if verify {
			script.WriteString(verifyRemoteFile(path, []byte(units[filename])))
		} else {
			script.WriteString(remoteFile(path, []byte(units[filename])))
		}
	}
	if !verify {
		script.WriteString("systemctl daemon-reload\nsystemctl enable --now " + shell(name+"-full.timer") + " " + shell(name+"-incr.timer") + "\n")
	}
	script.WriteString("systemctl is-active --quiet " + shell(name+"-full.timer") + "\nsystemctl is-active --quiet " + shell(name+"-incr.timer") + "\n")
	if _, err := r.remote(ctx, r.Plan, pl, script.String()); err != nil {
		return err
	}
	if verify {
		if _, err := r.remotePgBackRest(ctx, pl, "check"); err != nil {
			return fmt.Errorf("pgBackRest check: %w", err)
		}
	}
	return nil
}

func sortedFilesString(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
