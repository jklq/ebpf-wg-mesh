package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func recoveryConfig(path string) (recovery.Config, recovery.Service, error) {
	return recovery.Load(path)
}
func (r *Runner) effectiveConfig() string {
	return filepath.Join(r.Config.StateDirectory, r.Plan.ID, "recovery.json")
}
func (r *Runner) nativeRecovery(ctx context.Context, command string, args ...string) ([]byte, error) {
	var out strings.Builder
	err := deploy.RunRecovery(ctx, append([]string{command, "--config", r.effectiveConfig()}, args...), &out)
	return []byte(out.String()), err
}
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writePrivate(path, b)
}

func (r *Runner) prepareProtection(ctx context.Context) error {
	if !r.Plan.Recovery {
		if err := r.prepareInitialKeys(ctx); err != nil {
			return err
		}
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	c.Installation, c.Release, c.DatabaseURLFile, c.ConsoleSchema = r.Plan.Installation.ID, r.Plan.Release.ID, r.Config.Database.URLFile, r.Config.Console.Schema
	dir := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "inputs")
	var files []recovery.File
	add := func(kind, id, path string, secret bool) {
		files = append(files, recovery.File{Requirement: recovery.Requirement{Kind: kind, ID: id}, Path: path, Secret: secret})
	}
	for kind, value := range map[string]any{"installation": r.Plan.Installation, "release": r.Plan.Release} {
		path := filepath.Join(dir, kind+".json")
		if err := saveJSON(path, value); err != nil {
			return err
		}
		id := r.Plan.Installation.ID + "/" + r.Plan.ID
		if kind == "release" {
			id = r.Plan.Release.ID
		}
		add(kind, id, path, false)
	}
	statePath := filepath.Join(dir, "deployment-state.json")
	snapshot := deploy.State{Version: 1, InstallationID: r.Plan.Installation.ID, Generation: r.Plan.Generation, Policy: &r.Plan.Installation, Bundle: &r.Plan.Release, Placements: r.Plan.Placements}
	stateSource := os.Getenv("PLATFORM_DEPLOYMENT_STATE")
	if stateSource == "" {
		stateSource = "/etc/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/installer-state.json"
	}
	if b, err := os.ReadFile(stateSource); err == nil {
		if err := json.Unmarshal(b, &snapshot); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Inventory protection includes every process that could have started during
	// this applied plan, including interrupted cutovers. Restoration fences all
	// of them before creating a new empty database store.
	snapshot.Version = 1
	snapshot.InstallationID = r.Plan.Installation.ID
	snapshot.Generation = r.Plan.Generation
	snapshot.Policy = &r.Plan.Installation
	snapshot.Bundle = &r.Plan.Release
	snapshot.Placements = append([]deploy.Placement{}, r.Plan.Placements...)
	if r.Plan.Previous != nil {
		for _, old := range r.Plan.Previous.Placements {
			if !slices.Contains(snapshot.Placements, old) && !slices.Contains(snapshot.Retained, old) {
				snapshot.Retained = append(snapshot.Retained, old)
			}
		}
	}
	snapshot.Progress = nil
	if err := saveJSON(statePath, snapshot); err != nil {
		return err
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	add("deployment-state", r.Plan.Installation.ID+"/"+recovery.Digest(stateData), statePath, true)
	provider, err := secretkeys.NewKeyring(c.KeyringFile, secretkeys.KeyringOptions{})
	if err != nil {
		return err
	}
	versions, err := provider.LocalKeyVersions()
	if err != nil {
		return err
	}
	for _, version := range versions {
		add("keyring", version, c.KeyringFile, true)
	}
	key, err := os.ReadFile(r.Config.Console.TokenKeyFile)
	if err != nil {
		return err
	}
	add("console-key", "console-token/"+recovery.Digest(key), r.Config.Console.TokenKeyFile, true)
	// Recover file-based external credentials independently of the live vault.
	// The recovery decryption key is retained out of band and never sealed under itself.
	bundle := map[string][]byte{}
	for name, ref := range r.Plan.Installation.Secrets {
		if name == r.Plan.Installation.Backup.RecoveryKey || ref.File == r.Config.Database.URLFile || ref.File == r.Config.Console.TokenKeyFile || ref.File == c.KeyringFile {
			continue
		}
		if strings.Contains(ref.File, "{") {
			continue
		}
		b, err := os.ReadFile(ref.File)
		if err != nil {
			return fmt.Errorf("external credential %s unavailable: %w", name, err)
		}
		bundle[ref.File] = b
	}
	for _, path := range []string{r.Plan.Installation.OperationsConfig, r.Config.RecoveryConfig, c.Storage.CredentialsFile, r.Config.WildcardCertificate, r.Config.WildcardKey} {
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bundle[path] = b
	}
	b, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	secretPath := filepath.Join(dir, "external-secrets.json")
	if err := writePrivate(secretPath, b); err != nil {
		return err
	}
	add("external-secret", "installer-secrets/"+recovery.Digest(b), secretPath, true)
	// Additional explicitly versioned external service dependencies are retained.
	files = append(files, c.Files...)
	c.Files = files
	return saveJSON(r.effectiveConfig(), c)
}

func (r *Runner) protect(ctx context.Context, verify bool) error {
	if !verify {
		if err := r.prepareProtection(ctx); err != nil {
			return err
		}
		_, err := r.nativeRecovery(ctx, "protect-files")
		return err
	}
	_, err := r.nativeRecovery(ctx, "verify-files")
	return err
}
func (r *Runner) backupSchedule(ctx context.Context, verify bool) error {
	if !verify {
		if err := r.protect(ctx, false); err != nil {
			return err
		}
		// The backup URI is a private service selection, not lifecycle code.
		uri, err := os.ReadFile(r.Config.Database.BackupURIFile)
		if err != nil {
			return err
		}
		db, err := r.db(ctx, false)
		if err != nil {
			return err
		}
		c, s, err := recoveryConfig(r.effectiveConfig())
		if err != nil {
			db.Close()
			return err
		}
		clear(s.RecoveryKey)
		name := strings.TrimPrefix(c.BackupConnection, "external://")
		if name == "" || strings.ContainsAny(name, "/ -'\"") {
			db.Close()
			return fmt.Errorf("backup requires a named external connection")
		}
		statement := "CREATE EXTERNAL CONNECTION IF NOT EXISTS " + name + " AS '" + strings.ReplaceAll(strings.TrimSpace(string(uri)), "'", "''") + "'"
		_, err = db.ExecContext(ctx, statement)
		db.Close()
		if err != nil {
			return fmt.Errorf("native backup connection provisioning failed")
		}
		if _, err := r.nativeRecovery(ctx, "schedule"); err != nil {
			return err
		}
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	c, s, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	if err := recovery.VerifyBackupDestination(ctx, db, c.BackupConnection, c.Storage, c.BackupPrefix); err != nil {
		return err
	}
	if err := recovery.VerifySchedules(ctx, db, c.Installation, c.BackupConnection); err != nil {
		return err
	}
	if err := r.completionTimer(ctx, verify); err != nil {
		return err
	}
	// Connectivity is checked now; each completion publishes success/failure to
	// this independently hosted monitor. An unavailable monitor fails closed.
	return r.monitor(ctx, "installed")
}

func (r *Runner) completionTimer(ctx context.Context, verify bool) error {
	name := "platform-" + r.Plan.Installation.ID + "-recovery"
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	planPath := filepath.Join(r.Config.StateDirectory, "scheduled-plan.json")
	if !verify {
		if err := saveJSON(planPath, r.Plan); err != nil {
			return err
		}
	}
	// The OS timer owns protection refresh and point completion. Native SQL
	// schedules own database capture even when every platform process is down.
	service := "[Unit]\nDescription=Complete independent platform recovery point\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nExecStart=" + binary + " --plan " + planPath + " --config " + r.Plan.Installation.OperationsConfig + " backup-complete\n"
	timer := "[Unit]\nDescription=Independent platform recovery completion\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=1min\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	if verify {
		for ext, data := range map[string]string{"service": service, "timer": timer} {
			b, err := os.ReadFile("/etc/systemd/system/" + name + "." + ext)
			if err != nil || string(b) != data {
				return fmt.Errorf("recovery completion unit differs from selected release")
			}
		}
		return systemctl(ctx, "is-active", "--quiet", name+".timer")
	}
	for ext, data := range map[string]string{"service": service, "timer": timer} {
		if err := writePrivate("/etc/systemd/system/"+name+"."+ext, []byte(data)); err != nil {
			return err
		}
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return systemctl(ctx, "enable", "--now", name+".timer")
}
func (r *Runner) backup(ctx context.Context) error {
	if _, err := r.backupEvidence(ctx); err == nil {
		return nil
	}
	c, s, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	clear(s.RecoveryKey)
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "BACKUP INTO '"+strings.ReplaceAll(c.BackupConnection, "'", "''")+"' WITH revision_history"); err != nil {
		return fmt.Errorf("native full-cluster backup failed: %w", err)
	}
	return r.completeBackup(ctx)
}
func (r *Runner) completeBackup(ctx context.Context) (returnErr error) {
	defer func() {
		if returnErr != nil {
			_ = r.monitor(ctx, "failed")
		}
	}()
	args := []string{}
	if r.Config.SourceConfig != "" {
		args = append(args, "--source-config", r.Config.SourceConfig)
	}
	b, err := r.nativeRecovery(ctx, "complete", args...)
	if err != nil {
		return err
	}
	var evidence deploy.Evidence
	if err := json.Unmarshal(b, &evidence); err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(r.Config.StateDirectory, r.Plan.ID, "backup.json"), evidence); err != nil {
		return err
	}
	return r.monitor(ctx, "complete")
}
func (r *Runner) backupEvidence(ctx context.Context) (deploy.Evidence, error) {
	var e deploy.Evidence
	if err := privateJSON(filepath.Join(r.Config.StateDirectory, r.Plan.ID, "backup.json"), &e); err != nil {
		return e, err
	}
	if e.Point == nil || e.DataLossCutoff.Before(r.Plan.CreatedAt) || time.Since(e.DataLossCutoff) > recovery.Objective {
		return e, fmt.Errorf("complete backup is missing or stale")
	}
	b, err := r.nativeRecovery(ctx, "verify", "--backup", e.Backup, "--check-content")
	if err != nil {
		return e, err
	}
	var verified deploy.Evidence
	if err := json.Unmarshal(b, &verified); err != nil {
		return e, err
	}
	if !verified.DataLossCutoff.Equal(e.DataLossCutoff) {
		return e, fmt.Errorf("verified cutoff differs")
	}
	return verified, nil
}
func (r *Runner) finalize(ctx context.Context, verify bool) error {
	if !verify {
		_, err := r.nativeRecovery(ctx, "finalize")
		return err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	// The native finalizer owns the inventory format; verify its idempotent
	// resulting SQL rather than treating the invocation as a successful marker.
	c, s, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	clear(s.RecoveryKey)
	return recovery.VerifyFinishedRelease(ctx, db, c.Installation, c.Release)
}
func (r *Runner) monitor(ctx context.Context, status string) error {
	b, _ := json.Marshal(map[string]any{"installation": r.Plan.Installation.ID, "release": r.Plan.Release.ID, "status": status, "reportedAt": time.Now().UTC()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Plan.Installation.Backup.Monitor, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Config.MonitorTokenFile != "" {
		token, err := os.ReadFile(r.Config.MonitorTokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("independent monitor unavailable: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("independent monitor rejected report: HTTP %d", resp.StatusCode)
	}
	return nil
}
