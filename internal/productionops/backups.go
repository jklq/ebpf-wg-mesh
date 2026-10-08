package productionops

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

func (r *Runner) prepareProtection(ctx context.Context, selected *deploy.Release) error {
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
	c.Images.RegistryService = r.Config.RegistryService
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
	if selected == nil && r.Plan.Previous != nil && !r.Plan.Recovery {
		// An upgrade backup still belongs to the running release until SQL
		// conversion and new credentials have admitted the cutover.
		db, err := r.db(ctx, false)
		if err != nil {
			return fmt.Errorf("cannot determine the running release for protection: %w", err)
		}
		var previous, current bool
		var schema int
		err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM platform_recovery.public.installations WHERE installation=$1 AND release=$2), EXISTS(SELECT 1 FROM platform_recovery.public.installations WHERE installation=$1 AND release=$3)`, r.Plan.Installation.ID, r.Plan.Previous.Release.ID, r.Plan.Release.ID).Scan(&previous, &current)
		if err == nil {
			err = db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&schema)
		}
		db.Close()
		if err != nil {
			return err
		}
		if previous && !current && schema == r.Plan.Previous.Release.Schema {
			snapshot.Bundle = &r.Plan.Previous.Release
		} else if schema != r.Plan.Release.Schema {
			return fmt.Errorf("running schema has no unambiguous protected release")
		}
	}
	if selected != nil {
		snapshot.Bundle = selected
	}
	// The isolated drill must use the protected running release, including a
	// pre-conversion upgrade backup. The currently executing new release tool is
	// not necessarily the executable admitted at this SQL cutoff.
	tool, exists := snapshot.Bundle.Tools["operations"][runtime.GOARCH]
	if !exists || len(tool.SHA256) != 64 {
		return fmt.Errorf("selected release has no pinned native recovery executable")
	}
	c.DrillCommand = []string{"/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + snapshot.Bundle.ID + "/tools/operations", "offline-recovery"}
	c.DrillCommandDigest = "sha256:" + tool.SHA256
	// An independent completer may have selected a prior release's storage
	// helpers. The applied protection snapshot owns the executable selection,
	// including the old release for a pre-conversion recovery point.
	c.Images.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + snapshot.Bundle.ID + "/tools/skopeo"
	policy := r.Plan.Installation
	policy.Release = snapshot.Bundle.ID
	snapshot.Policy = &policy
	if err := saveJSON(filepath.Join(dir, "installation.json"), policy); err != nil {
		return err
	}
	snapshot.Placements = append([]deploy.Placement{}, r.Plan.Placements...)
	if r.Plan.Previous != nil {
		for _, old := range r.Plan.Previous.Placements {
			if !slices.Contains(snapshot.Placements, old) && !slices.Contains(snapshot.Retained, old) {
				snapshot.Retained = append(snapshot.Retained, old)
			}
		}
	}
	if snapshot.Bindings == nil {
		snapshot.Bindings = map[string]deploy.Binding{}
	}
	for _, host := range r.Plan.Installation.Hosts {
		if _, exists := snapshot.Bindings[host.ID]; !exists && host.Binding.ServerID != "" {
			snapshot.Bindings[host.ID] = host.Binding
		}
	}
	snapshot.Progress = nil
	if err := saveJSON(statePath, snapshot); err != nil {
		return err
	}

	if snapshot.Bundle.ID != r.Plan.Release.ID {
		path := filepath.Join(dir, "release.json")
		if err := saveJSON(path, *snapshot.Bundle); err != nil {
			return err
		}
		for n := range files {
			if files[n].Requirement.Kind == "release" {
				files[n].Requirement.ID = snapshot.Bundle.ID
			}
		}
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
	bundle, err := r.externalInputs(c)
	if err != nil {
		return err
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
		if err := r.prepareProtection(ctx, nil); err != nil {
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
		if err := r.stopPriorMaintenance(ctx); err != nil {
			return err
		}
		if err := r.prepareProtection(ctx, &r.Plan.Release); err != nil {
			return err
		}
		if _, err := r.nativeRecovery(ctx, "protect-files"); err != nil {
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
		if c.Storage.CAFile != "" {
			ca, readErr := os.ReadFile(c.Storage.CAFile)
			pool := x509.NewCertPool()
			if readErr != nil || !pool.AppendCertsFromPEM(ca) {
				db.Close()
				return fmt.Errorf("invalid backup storage TLS CA")
			}
			if _, err := db.ExecContext(ctx, "SET CLUSTER SETTING cloudstorage.http.custom_ca = $1", string(ca)); err != nil {
				db.Close()
				return err
			}
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
	if c.Storage.CAFile != "" {
		ca, err := os.ReadFile(c.Storage.CAFile)
		if err != nil {
			return err
		}
		var actual string
		if err := db.QueryRowContext(ctx, "SHOW CLUSTER SETTING cloudstorage.http.custom_ca").Scan(&actual); err != nil {
			return err
		}
		if actual != string(ca) {
			return fmt.Errorf("native backup storage TLS trust differs from selected CA")
		}
	}
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
	if err := r.captureExternalInventory(ctx); err != nil {
		return err
	}
	if err := r.protect(ctx, false); err != nil {
		return err
	}

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
		if err := r.admitReservations(ctx); err != nil {
			return err
		}
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
