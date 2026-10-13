package deploy

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/platformdb"
)

// RunDatabaseStatus observes the single PostgreSQL primary over its
// operator connection and reports DatabaseStatus JSON. It replaces the
// CockroachDB node/range inspection: there is no membership to converge,
// only a primary that must be live, out of recovery, and archiving WAL.
// A release can use this command as its database-verify hook without a
// platform login.
func RunDatabaseStatus(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("platformctl database-status", flag.ContinueOnError)
	var planPath, urlFile string
	var requireLive bool
	fs.StringVar(&planPath, "plan", os.Getenv("PLATFORM_PLAN"), "installation plan JSON")
	fs.StringVar(&urlFile, "url-file", "", "private file holding the operator database URL")
	fs.BoolVar(&requireLive, "require-live", false, "fail until the planned primary is live and archiving WAL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || planPath == "" || !filepath.IsAbs(urlFile) {
		return fmt.Errorf("database-status requires --plan and --url-file")
	}
	b, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	primary := ""
	for _, pl := range p.Placements {
		if pl.Role == Database {
			if primary != "" {
				return fmt.Errorf("plan places more than one database primary")
			}
			primary = pl.Host
		}
	}
	raw, err := readSecretFile("operator-database", urlFile)
	if err != nil {
		return err
	}
	status, err := inspectPrimary(ctx, strings.TrimSpace(string(raw)), primary)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(status); err != nil {
		return err
	}
	if requireLive {
		if primary == "" || len(status.Live) != 1 || status.Live[0] != primary {
			return fmt.Errorf("database primary %s is not live", primary)
		}
		if !status.Archiving {
			return fmt.Errorf("database primary is not archiving WAL")
		}
	}
	return nil
}

// inspectPrimary runs the observation query. A connection or query failure
// is reported as an empty (not live) status only when requireLive callers
// can still inspect it: here it stays an error, because a verifier that
// cannot reach the primary has observed nothing.
func inspectPrimary(ctx context.Context, rawURL, primary string) (DatabaseStatus, error) {
	status := DatabaseStatus{Versions: map[string]string{}}
	if primary != "" {
		status.Members = []string{primary}
	}
	db, err := platformdb.OpenURL(ctx, rawURL, platformdb.SchemaPlatform)
	if err != nil {
		return status, err
	}
	defer db.Close()
	var version, walSegment, archiveMode string
	var inRecovery bool
	var timeline int
	err = db.QueryRowContext(ctx, `SELECT current_setting('server_version'),
		pg_is_in_recovery(),
		COALESCE(pg_walfile_name(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END), ''),
		(SELECT timeline_id FROM pg_control_checkpoint()),
		current_setting('archive_mode')`).Scan(&version, &inRecovery, &walSegment, &timeline, &archiveMode)
	if err != nil {
		return status, fmt.Errorf("inspect PostgreSQL primary: %w", err)
	}
	status.Timeline = timeline
	status.WALSegment = walSegment
	status.InRecovery = inRecovery
	if primary != "" {
		status.Versions[primary] = version
		if !inRecovery {
			status.Live = []string{primary}
		}
	}
	var lastArchived string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_archived_wal,'') FROM pg_stat_archiver`).Scan(&lastArchived); err != nil {
		return status, err
	}
	status.Archiving = (archiveMode == "on" || archiveMode == "always") && lastArchived != ""
	return status, nil
}
