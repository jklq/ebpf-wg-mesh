package bootstrap

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// keysSchemaVersion is the minimum control-plane schema version carrying the
// envelope key tables. The operator CLI refuses to run against older
// databases instead of failing mid-rotation.
const keysSchemaVersion = 22

// RunKeys implements `controlplane keys`, the operator command surface for
// envelope key lifecycle:
//
//	list                 show key IDs, states, and wrapped DEK counts
//	rotate               introduce a new active key (retires the previous one)
//	rewrap               re-wrap existing data-encryption keys onto the active key
//	delete --key-id ID   delete a retired, unreferenced key
//
// Rotation is two steps by design: rotate, then rewrap until it reports
// zero. Retired keys keep unwrapping, so workloads are unaffected between
// the two steps.
func RunKeys(args []string) error {
	if len(args) == 0 {
		return keysUsageError()
	}
	command, rest := args[0], args[1:]
	switch command {
	case "list", "rotate", "rewrap", "delete":
		return runKeysCommand(command, rest)
	default:
		return keysUsageError()
	}
}

func keysUsageError() error {
	return fmt.Errorf("usage: controlplane keys <list|rotate|rewrap|delete> [flags]")
}

func runKeysCommand(command string, args []string) error {
	var dbURL, stateDir, keyID, newKMSKeyID string
	var keysCfg config.SecretKeysConfig
	fs := flag.NewFlagSet("controlplane keys "+command, flag.ContinueOnError)
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &stateDir, "state-dir", "CONTROLPLANE_STATE_DIR", "var/controlplane", "")
	stringFlag(fs, &keysCfg.Provider, "secret-keys-provider", "CONTROLPLANE_SECRET_KEYS_PROVIDER", "file", "file or aws-kms")
	stringFlag(fs, &keysCfg.File.Directory, "secret-keys-dir", "CONTROLPLANE_SECRET_KEYS_DIR", "", "defaults under the state directory")
	stringFlag(fs, &keysCfg.KMS.Region, "secret-keys-kms-region", "CONTROLPLANE_SECRET_KEYS_KMS_REGION", "", "")
	stringFlag(fs, &keysCfg.KMS.Endpoint, "secret-keys-kms-endpoint", "CONTROLPLANE_SECRET_KEYS_KMS_ENDPOINT", "", "")
	stringFlag(fs, &keysCfg.KMS.KeyID, "secret-keys-kms-key-id", "CONTROLPLANE_SECRET_KEYS_KMS_KEY_ID", "", "")
	intFlag(fs, &keysCfg.KMS.TimeoutSeconds, "secret-keys-kms-timeout-seconds", "CONTROLPLANE_SECRET_KEYS_KMS_TIMEOUT_SECONDS", 10, "")
	intFlag(fs, &keysCfg.KMS.MaxAttempts, "secret-keys-kms-max-attempts", "CONTROLPLANE_SECRET_KEYS_KMS_MAX_ATTEMPTS", 5, "")
	if command == "delete" {
		stringFlag(fs, &keyID, "key-id", "CONTROLPLANE_KEYS_KEY_ID", "", "envelope key ID to delete")
	}
	if command == "rotate" {
		stringFlag(fs, &newKMSKeyID, "new-kms-key-id", "CONTROLPLANE_KEYS_NEW_KMS_KEY_ID", "", "new KMS key for aws-kms rotation (required for aws-kms)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(dbURL) == "" {
		return fmt.Errorf("controlplane keys: db-url is required")
	}
	if strings.TrimSpace(keysCfg.Provider) == "" {
		keysCfg.Provider = config.SecretKeysProviderFile
	}
	if keysCfg.Provider == config.SecretKeysProviderFile && strings.TrimSpace(keysCfg.File.Directory) == "" {
		keysCfg.File.Directory = filepath.Join(stateDir, "secret-keys")
	}
	if keysCfg.KMS.TimeoutSeconds <= 0 || keysCfg.KMS.MaxAttempts <= 0 {
		return fmt.Errorf("controlplane keys: kms timeout and attempts must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("controlplane keys: open database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("controlplane keys: ping database: %w", err)
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("controlplane keys: read schema version: %w", err)
	}
	if version < keysSchemaVersion {
		return fmt.Errorf("controlplane keys: database schema v%d is older than envelope keys v%d; start the control plane first",
			version, keysSchemaVersion)
	}
	provider, err := secretkeys.OpenProvider(ctx, keysCfg)
	if err != nil {
		return fmt.Errorf("controlplane keys: %w", err)
	}
	defer provider.Close()
	svc := secretkeys.New(db, provider)

	switch command {
	case "list":
		return keysList(ctx, svc)
	case "rotate":
		return keysRotate(ctx, svc, keysCfg.Provider, newKMSKeyID)
	case "rewrap":
		return keysRewrap(ctx, svc)
	case "delete":
		if strings.TrimSpace(keyID) == "" {
			return fmt.Errorf("controlplane keys delete: --key-id is required")
		}
		return keysDelete(ctx, svc, keyID)
	default:
		return keysUsageError()
	}
}

func keysList(ctx context.Context, svc *secretkeys.Service) error {
	keys, err := svc.Registry().ListKeys(ctx)
	if err != nil {
		return err
	}
	counts, err := svc.Registry().WrappedCounts(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "provider: %s\n", svc.ProviderName())
	if len(keys) == 0 {
		fmt.Fprintln(os.Stdout, "no envelope keys")
		return nil
	}
	fmt.Fprintln(os.Stdout, "ID\tSTATE\tWRAPPED DEKS\tPROVIDER REF\tCREATED")
	for _, key := range keys {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%s\t%s\n",
			key.ID, key.State, counts[key.ID], key.ProviderRef, key.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func keysRotate(ctx context.Context, svc *secretkeys.Service, provider, newKMSKeyID string) error {
	if provider == config.SecretKeysProviderAWSKMS && strings.TrimSpace(newKMSKeyID) == "" {
		return fmt.Errorf("controlplane keys rotate: --new-kms-key-id is required for aws-kms")
	}
	rec, err := svc.Registry().Rotate(ctx, newKMSKeyID)
	if err != nil {
		return fmt.Errorf("controlplane keys rotate: %w", err)
	}
	fmt.Fprintf(os.Stdout, "active key: %s (provider ref %s)\n", rec.ID, rec.ProviderRef)
	fmt.Fprintln(os.Stdout, "previous active key retired; run `controlplane keys rewrap` to migrate wrapped data-encryption keys")
	return nil
}

func keysRewrap(ctx context.Context, svc *secretkeys.Service) error {
	rewrapped, err := svc.DEKs().RewrapAll(ctx)
	if err != nil {
		return fmt.Errorf("controlplane keys rewrap: %w", err)
	}
	fmt.Fprintf(os.Stdout, "rewrapped %d data-encryption key(s) onto the active key\n", rewrapped)
	return nil
}

func keysDelete(ctx context.Context, svc *secretkeys.Service, keyID string) error {
	if err := svc.Registry().DeleteKey(ctx, keyID); err != nil {
		return fmt.Errorf("controlplane keys delete: %w", err)
	}
	fmt.Fprintf(os.Stdout, "deleted envelope key %s\n", strings.TrimSpace(keyID))
	return nil
}
