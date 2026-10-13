package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func (r *Runner) planFile() string {
	if p := os.Getenv("PLATFORM_PLAN"); p != "" {
		return p
	}
	return filepath.Join(r.Config.StateDirectory, r.Plan.ID, "plan.json")
}
func (r *Runner) progressFile() string {
	if p := os.Getenv("PLATFORM_RECOVERY_OPERATION"); p != "" {
		return p
	}
	return filepath.Join(r.Config.StateDirectory, r.Plan.ID, "progress.json")
}
func (r *Runner) selectedPoint(ctx context.Context) (deploy.Evidence, error) {
	var e deploy.Evidence
	if !r.Plan.Recovery {
		return e, fmt.Errorf("selected point requires a restore plan")
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return e, err
	}
	defer clear(s.RecoveryKey)
	selected := os.Getenv("PLATFORM_BACKUP")
	if selected == "" {
		return e, fmt.Errorf("restore requires an independently recorded exact recovery point")
	}
	o, err := s.ResolvePoint(ctx, c.Storage.Bucket, selected)
	if err != nil {
		return e, err
	}
	p, err := s.ReadPoint(ctx, o)
	if err != nil {
		return e, err
	}
	if p.Installation != r.Plan.Installation.ID {
		return e, fmt.Errorf("recovery point belongs to another installation")
	}
	requested, err := time.Parse(time.RFC3339Nano, os.Getenv("PLATFORM_DATA_LOSS_CUTOFF"))
	if err != nil || !requested.Equal(p.Snapshot.Timestamp) {
		return e, fmt.Errorf("restore cutoff differs from exact selected timestamp")
	}
	report := s.Verify(ctx, p, true)
	if !report.Complete {
		return e, fmt.Errorf("recovery point dependencies failed: %v %v", report.Missing, report.Failures)
	}
	found := false
	for _, d := range p.Dependencies {
		if d.Kind == "release" && d.ID == r.Plan.Release.ID {
			path := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "selected-release.json")
			if err := s.Materialize(ctx, d, path, false); err != nil {
				return e, err
			}
			var release deploy.Release
			if err := privateJSON(path, &release); err != nil {
				return e, err
			}
			if deploy.Digest(release) != deploy.Digest(r.Plan.Release) {
				return e, fmt.Errorf("restore release differs from protected release")
			}
			found = true
		}
	}
	if !found {
		return e, fmt.Errorf("selected release is not protected")
	}
	return deploy.Evidence{Backup: o.S3URL(c.Storage.Bucket), Object: &o, Point: &p, DataLossCutoff: p.Snapshot.Timestamp}, nil
}
func (r *Runner) recoverSecrets(ctx context.Context, p recovery.Point, s recovery.Service, c recovery.Config) error {
	// Recover independent ciphertext before opening any restored envelope keys.
	if err := s.RecoverKeyring(ctx, p, c.KeyringFile); err != nil {
		return err
	}
	selectedKey := ""
	for _, d := range p.Dependencies {
		if d.Kind == "console-key" {
			if selectedKey != "" && selectedKey != d.ID {
				return fmt.Errorf("multiple console token keys require an explicit selected active key")
			}
			selectedKey = d.ID
			if err := s.Materialize(ctx, d, r.Config.Console.TokenKeyFile, true); err != nil {
				return err
			}
		}
	}
	if selectedKey == "" {
		return fmt.Errorf("console token key was not protected")
	}

	return nil
}
func (r *Runner) restore(ctx context.Context) error {
	if err := r.fence(ctx, true); err != nil {
		return err
	}
	e, err := r.selectedPoint(ctx)
	if err != nil {
		return err
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	if err := r.recoverSecrets(ctx, *e.Point, s, c); err != nil {
		return err
	}
	pl, err := r.databasePlacement()
	if err != nil {
		return err
	}
	// A completed restore is its own retry marker: the restored database
	// verifies against the selected point exactly.
	if err := r.verifyRestore(ctx); err == nil {
		return nil
	}
	data := dataDir(r.Plan, pl) + "/data"
	marker := dataDir(r.Plan, pl) + "/restore-" + r.Plan.ID
	started, err := r.remote(ctx, r.Plan, pl, "if test -f "+shell(marker)+"; then printf started; else printf fresh; fi\n")
	if err != nil {
		return err
	}
	if string(started) != "started" {
		// Only the freshly initialized empty cluster may be replaced.
		if err := r.verifyEmptyDatabase(ctx); err != nil {
			return err
		}
	}
	repoRoot := dataDir(r.Plan, pl) + "/recovery-repo"
	for _, o := range e.Point.Database.Objects {
		rel := strings.TrimPrefix(o.Key, strings.Trim(c.BackupPrefix, "/")+"/")
		if rel == o.Key || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("backup repository object escapes the selected repository")
		}
		f, err := os.CreateTemp(r.Config.StateDirectory, ".backup-object-")
		if err != nil {
			return err
		}
		path := f.Name()
		f.Close()
		if err := s.Storage.Get(ctx, o, path); err != nil {
			os.Remove(path)
			return err
		}
		digest, size, err := recovery.FileDigest(path)
		if err != nil || digest != o.Digest || size != o.Size {
			os.Remove(path)
			return fmt.Errorf("frozen backup object failed its exact-version digest")
		}
		host, err := r.host(ctx, r.Plan, pl.Host)
		if err != nil {
			os.Remove(path)
			return err
		}
		err = r.Remote.Upload(ctx, r.Plan.Installation, host, path, repoRoot+"/"+rel, strings.TrimPrefix(digest, "sha256:"))
		os.Remove(path)
		if err != nil {
			return err
		}
	}
	restoreConfig := recovery.RenderPgBackRestPosixConfig(r.Config.Database.PgBackRestStanza, data, repoRoot)
	script := remoteFile(cfgDir(r.Plan, pl)+"/certs/pgbackrest-restore.conf", []byte(restoreConfig))
	script += "chown " + shell(deploy.DatabaseOSUser) + " " + shell(cfgDir(r.Plan, pl)+"/certs/pgbackrest-restore.conf") + "\n"
	script += "chown -R " + shell(deploy.DatabaseOSUser) + " " + shell(repoRoot) + "\n"
	script += remoteFile(marker, []byte(e.Point.Database.BackupLabel+"\n"))
	script += "systemctl stop " + shell(unit(r.Plan, pl)) + "\n"
	script += "rm -rf " + shell(data) + "\nmkdir -p " + shell(data) + "\nchown " + shell(deploy.DatabaseOSUser) + " " + shell(data) + "\nchmod 0700 " + shell(data) + "\n"
	restoreArgs := []string{
		shell(r.databaseTool("pgbackrest")),
		"--config=" + shell(cfgDir(r.Plan, pl)+"/certs/pgbackrest-restore.conf"),
		"--stanza=" + shell(r.Config.Database.PgBackRestStanza),
		"restore", "--set=" + shell(e.Point.Database.BackupLabel),
		"--type=time", "--target=" + shell(e.DataLossCutoff.UTC().Format("2006-01-02 15:04:05.999999-07")),
		"--target-action=promote",
	}
	script += "runuser -u " + shell(deploy.DatabaseOSUser) + " -- " + strings.Join(restoreArgs, " ") + "\n"
	script += "systemctl start " + shell(unit(r.Plan, pl)) + "\n"
	if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
		return fmt.Errorf("pgBackRest restore failed; destination is preserved for inspection: %w", err)
	}
	if err := r.waitPromoted(ctx); err != nil {
		return err
	}
	return r.verifyRestore(ctx)
}

// waitPromoted waits until WAL replay reaches the recovery target and the
// primary promotes out of recovery.
func (r *Runner) waitPromoted(ctx context.Context) error {
	for {
		db, err := r.db(ctx, true)
		if err == nil {
			var inRecovery bool
			err = db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery)
			db.Close()
			if err == nil && !inRecovery {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("restored primary did not promote: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (r *Runner) verifyRestore(ctx context.Context) error {
	e, err := r.selectedPoint(ctx)
	if err != nil {
		return err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var inRecovery bool
	if err := db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return err
	}
	if inRecovery {
		return fmt.Errorf("point-in-time restore has not promoted")
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	clear(s.RecoveryKey)
	return recovery.CheckRestoredDatabase(ctx, db, *e.Point, r.Config.Console.Schema, c.KeyringFile, []string{r.Config.Console.TokenKeyFile})
}

func (r *Runner) authority(ctx context.Context, verify bool) error {
	if !r.Plan.Recovery {
		return fmt.Errorf("authority replacement is restricted to a fenced restore")
	}
	if err := r.fence(ctx, true); err != nil {
		return err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		return err
	}
	defer keys.Close()
	if !verify {
		if err := signing.ResetForRecovery(ctx, r.Plan.Installation.ID, r.Plan.Generation); err != nil {
			return err
		}
		// Refresh sessions are bearer credentials from the lost authority. Access
		// tokens also cease to validate because signing keys have no overlap.
		if _, err := db.ExecContext(ctx, "DELETE FROM "+r.Config.Console.Schema+".refresh_sessions"); err != nil {
			return err
		}
		if err := r.exportSigning(ctx, signing); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `UPDATE build_scheduler_control SET paused=TRUE,updated_at=statement_timestamp() WHERE id=TRUE`); err != nil {
			return err
		}
		if err := r.reservations(ctx, false); err != nil {
			return err
		}
		// Fencing permits the recovery transition to replace retained host
		// admission. Credential issuance and renewal only verify that identity.
		// Keep processes stopped until their subsequent runtime phases.
		if err := r.installAdmission(ctx, true, false, false); err != nil {
			return err
		}
		return r.credentials(ctx, false)
	}
	var id, generation string
	var paused bool
	if err := db.QueryRowContext(ctx, `SELECT installation,generation,paused FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&id, &generation, &paused); err != nil {
		return err
	}
	if id != r.Plan.Installation.ID || generation != r.Plan.Generation || !paused {
		return fmt.Errorf("new authority must remain paused")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM platform_signing_keys WHERE state<>'active'`).Scan(&count); err != nil || count != 0 {
		return fmt.Errorf("old signing credentials still overlap")
	}
	e, err := r.selectedPoint(ctx)
	if err != nil {
		return err
	}
	for _, old := range e.Point.Snapshot.Identities["platform_signing_keys"] {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM platform_signing_keys WHERE id=$1`, old).Scan(&count); err != nil || count != 0 {
			return fmt.Errorf("prior signing key survived replacement")
		}
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+r.Config.Console.Schema+".refresh_sessions").Scan(&count); err != nil || count != 0 {
		return fmt.Errorf("console refresh sessions were not invalidated")
	}
	return r.credentials(ctx, true)
}

func (r *Runner) writeNativeInputs() error {
	if err := saveJSON(r.planFile(), r.Plan); err != nil {
		return err
	}
	if r.RecoveryProgress != nil {
		return saveJSON(r.progressFile(), r.RecoveryProgress)
	}
	return nil
}

// Keep decoding here explicit: recovery material is data, never shell code.
func decodeDocument(b []byte, target any) error { return json.Unmarshal(b, target) }
