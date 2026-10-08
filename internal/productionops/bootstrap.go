package productionops

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/deploy"
)

func (r *Runner) keys(ctx context.Context, db *sql.DB) (*secretkeys.Service, *signkeys.Service, error) {
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return nil, nil, err
	}
	clear(s.RecoveryKey)
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: c.KeyringFile}, secretkeys.Options{})
	if err != nil {
		return nil, nil, err
	}
	return keys, signkeys.New(db, keys.Registry()), nil
}
func (r *Runner) prepareInitialKeys(ctx context.Context) error {
	if r.Plan.Recovery {
		return fmt.Errorf("restore never generates missing envelope keys")
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	if r.Plan.Previous != nil {
		if _, err := os.Stat(c.KeyringFile); err != nil {
			return fmt.Errorf("upgrade requires existing envelope keys: %w", err)
		}
		if _, err := os.Stat(r.Config.Console.TokenKeyFile); err != nil {
			return err
		}
	}
	keyring, err := secretkeys.NewKeyring(c.KeyringFile, secretkeys.KeyringOptions{AllowGenerate: r.Plan.Previous == nil})
	if err != nil {
		return err
	}
	if _, err := keyring.EnsureBootstrapKey(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(r.Config.Console.TokenKeyFile); os.IsNotExist(err) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		defer clear(b)
		return writePrivate(r.Config.Console.TokenKeyFile, []byte(base64.StdEncoding.EncodeToString(b)))
	} else {
		return err
	}
}
func (r *Runner) bootstrap(ctx context.Context) error {
	if r.Plan.Recovery || r.Plan.Previous != nil {
		return fmt.Errorf("bootstrap is restricted to a fresh installation")
	}
	if err := r.verifyFreshDatabaseReady(ctx); err != nil {
		return err
	}
	if err := r.protect(ctx, true); err != nil {
		return err
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := controlplane.BootstrapInstallation(ctx, db, c.KeyringFile); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recovery_runtime_authority(singleton,installation,generation,paused) VALUES (TRUE,$1,$2,FALSE) ON CONFLICT DO NOTHING`, r.Plan.Installation.ID, r.Plan.Generation); err != nil {
		return err
	}
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		return err
	}
	defer keys.Close()
	if err := r.exportSigning(ctx, signing); err != nil {
		return err
	}
	return r.consoleAdmin(ctx, "bootstrap")
}
func (r *Runner) exportSigning(ctx context.Context, s *signkeys.Service) error {
	for _, scope := range []string{signkeys.ScopeDashboardSession, signkeys.ScopeUserAssertion} {
		b, err := s.ActiveSecret(ctx, scope)
		if err != nil {
			return err
		}
		if err := writePrivate(filepath.Join(r.Config.StateDirectory, scope+".key"), b); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runner) consoleAdmin(ctx context.Context, action string) error {
	data, err := os.ReadFile(r.Config.Database.URLFile)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(string(data))
	if err != nil {
		return err
	}
	selectDatabaseHost(parsed, r.reachableDatabase(ctx))
	adminURL := filepath.Join(r.Config.StateDirectory, "console-admin-url")
	if err = writePrivate(adminURL, []byte(parsed.String())); err != nil {
		return err
	}
	input := map[string]any{"databaseURLFile": adminURL, "schema": r.Config.Console.Schema, "tokenKeyFile": r.Config.Console.TokenKeyFile, "sessionKeyFile": filepath.Join(r.Config.StateDirectory, signkeys.ScopeDashboardSession+".key"), "installation": r.Plan.Installation.ID, "generation": r.Plan.Generation}
	if action == "check-endpoints" {
		var endpoints []map[string]string
		for _, endpoint := range r.Plan.Installation.Endpoints {
			if endpoint.Role == deploy.Console {
				endpoints = append(endpoints, map[string]string{"url": endpoint.URL, "caFile": r.Config.EndpointProbes[endpoint.Name].CAFile})
			}
		}
		input["endpoints"] = endpoints
	}
	b, _ := json.Marshal(input)
	cmd := exec.CommandContext(ctx, r.Config.Console.AdminBinary, action)
	cmd.Stdin = strings.NewReader(string(b))
	var output privateCommandOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		path := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "native-console-"+action+".log")
		if writeErr := writePrivate(path, output.Bytes()); writeErr != nil {
			return fmt.Errorf("native console %s failed: %w; private diagnostics unavailable: %v", action, err, writeErr)
		}
		return fmt.Errorf("native console %s failed: %w (private diagnostics: %s)", action, err, path)
	}
	return nil
}
func (r *Runner) verifyBootstrap(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var version, count int
	var installation, generation string
	if err := db.QueryRowContext(ctx, `SELECT installation,generation FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&installation, &generation); err != nil {
		return err
	}
	if installation != r.Plan.Installation.ID || generation != r.Plan.Generation {
		return fmt.Errorf("bootstrap authority belongs to another installation generation")
	}
	if err := db.QueryRowContext(ctx, `SELECT max(version),count(*) FROM schema_migrations`).Scan(&version, &count); err != nil {
		return err
	}
	if count != 1 || version != r.Plan.Release.Schema {
		return fmt.Errorf("platform schema differs from release")
	}
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		return err
	}
	defer keys.Close()
	if err := keys.Registry().VerifyLocalCoverage(ctx); err != nil {
		return err
	}
	if _, err := keys.DEKs().VerifyAll(ctx); err != nil {
		return err
	}
	if !signing.Ready(ctx, signkeys.AllScopes()) {
		return fmt.Errorf("signing scopes are incomplete")
	}
	return r.consoleAdmin(ctx, "check")
}
func clusterID(ca []byte) string {
	b := sha256.Sum256([]byte(strings.TrimSpace(string(ca))))
	return hex.EncodeToString(b[:])
}

// Host-admin changes use the same product journal as the running platform, so
// existing replicas observe reservations and enrollment without restart races.
func adminTransaction(ctx context.Context, db *sql.DB, fn func(context.Context, *sql.Tx) error) error {
	j := journal.New(db, "default", func(ctx context.Context, tx *sql.Tx) (int64, error) {
		var epoch int64
		err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id=1 FOR UPDATE`).Scan(&epoch)
		return epoch, err
	})
	_, err := j.Execute(ctx, fn)
	return err
}

// Native tool diagnostics can include private service identifiers. Bound their
// retained size and keep them in the operator-only state directory.
type privateCommandOutput struct{ bytes.Buffer }

func (b *privateCommandOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}
