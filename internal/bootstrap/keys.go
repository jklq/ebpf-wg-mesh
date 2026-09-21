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
const keysSchemaVersion = 23

// RunKeys implements `controlplane keys`, the operator command surface for
// envelope key lifecycle:
//
//	list                 show key IDs, states, wrapped DEK counts, and local material
//	provision            mint a new master-key version into the local keyring file
//	activate             record an already-provisioned version as the active key
//	rewrap               re-wrap existing data-encryption keys onto the active key
//	delete --key-id ID   delete a retired, unreferenced key
//	check                verify this replica holds every recorded version and every DEK unwraps
//
// Rotation is provision, then activate, then rewrap until it reports zero:
// provision mints the version into one keyring file, the operator copies
// that file to every replica (and runs `check --key-id` on each), activate
// retires the previous key for new writes, and rewrap migrates wrapped DEKs
// resumably. Retired keys keep unwrapping, so workloads are unaffected
// between the steps.
func RunKeys(args []string) error {
	if len(args) == 0 {
		return keysUsageError()
	}
	command, rest := args[0], args[1:]
	switch command {
	case "list", "provision", "activate", "rewrap", "delete", "check":
		return runKeysCommand(command, rest)
	default:
		return keysUsageError()
	}
}

func keysUsageError() error {
	return fmt.Errorf("usage: controlplane keys <list|provision|activate|rewrap|delete|check> [flags]")
}

func runKeysCommand(command string, args []string) error {
	var dbURL, stateDir, keyID string
	var keysCfg config.SecretKeysConfig
	fs := flag.NewFlagSet("controlplane keys "+command, flag.ContinueOnError)
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &stateDir, "state-dir", "CONTROLPLANE_STATE_DIR", "var/controlplane", "")
	stringFlag(fs, &keysCfg.KeyringPath, "secret-keys-keyring", "CONTROLPLANE_SECRET_KEYS_KEYRING", "", "provisioned master-key ring file")
	if command == "delete" || command == "activate" {
		stringFlag(fs, &keyID, "key-id", "CONTROLPLANE_KEYS_KEY_ID", "", "key version (activate) or envelope key ID (delete)")
	}
	if command == "provision" {
		stringFlag(fs, &keyID, "key-id", "CONTROLPLANE_KEYS_KEY_ID", "", "optional key version ID; generated when empty")
	}
	if command == "check" {
		stringFlag(fs, &keyID, "key-id", "CONTROLPLANE_KEYS_KEY_ID", "", "optional single version to check on this replica")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(keysCfg.KeyringPath) == "" {
		keysCfg.KeyringPath = filepath.Join(stateDir, "secret-keys", "keys.json")
	}

	// Provision touches only the local keyring file: it needs no database
	// and never generates database state as a side effect.
	if command == "provision" {
		return keysProvision(keysCfg, keyID)
	}
	if command == "activate" && strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("controlplane keys activate: --key-id is required")
	}
	if command == "delete" && strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("controlplane keys delete: --key-id is required")
	}

	if strings.TrimSpace(dbURL) == "" {
		return fmt.Errorf("controlplane keys: db-url is required")
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
	// The CLI never auto-generates: provision is the explicit command for
	// that, and every other command fails closed on missing material.
	provider, err := secretkeys.OpenProvider(keysCfg, secretkeys.Options{})
	if err != nil {
		return fmt.Errorf("controlplane keys: %w", err)
	}
	defer provider.Close()
	svc := secretkeys.New(db, provider)

	switch command {
	case "list":
		return keysList(ctx, svc)
	case "activate":
		return keysActivate(ctx, svc, keyID)
	case "rewrap":
		return keysRewrap(ctx, svc)
	case "delete":
		return keysDelete(ctx, svc, keyID)
	case "check":
		return keysCheck(ctx, svc, keyID)
	default:
		return keysUsageError()
	}
}

func keysProvision(keysCfg config.SecretKeysConfig, keyID string) error {
	provider, err := secretkeys.OpenProvider(keysCfg, secretkeys.Options{})
	if err != nil {
		return fmt.Errorf("controlplane keys provision: %w", err)
	}
	defer provider.Close()
	keyring, ok := provider.(*secretkeys.Keyring)
	if !ok {
		return fmt.Errorf("controlplane keys provision: provider %s cannot provision versions", provider.Name())
	}
	version, err := keyring.GenerateKey(context.Background(), keyID)
	if err != nil {
		return fmt.Errorf("controlplane keys provision: %w", err)
	}
	fmt.Fprintf(os.Stdout, "provisioned key version %s in %s\n", version, keyring.Path())
	fmt.Fprintln(os.Stdout, "copy this keyring file to every replica (and back it up separately from the database), then run `controlplane keys activate --key-id "+version+"`")
	return nil
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
	local := map[string]bool{}
	if keyring, ok := svc.Provider().(*secretkeys.Keyring); ok {
		versions, err := keyring.LocalKeyVersions()
		if err != nil {
			fmt.Fprintf(os.Stdout, "warning: cannot read local keyring %s: %v\n", keyring.Path(), err)
		} else {
			for _, version := range versions {
				local[version] = true
			}
		}
	}
	fmt.Fprintf(os.Stdout, "provider: %s\n", svc.ProviderName())
	if len(keys) == 0 {
		fmt.Fprintln(os.Stdout, "no envelope keys")
		return nil
	}
	fmt.Fprintln(os.Stdout, "ID\tSTATE\tWRAPPED DEKS\tLOCAL\tVERSION\tCREATED")
	for _, key := range keys {
		presence := "no"
		if local[key.ProviderRef] {
			presence = "yes"
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%s\t%s\t%s\n",
			key.ID, key.State, counts[key.ID], presence, key.ProviderRef, key.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func keysActivate(ctx context.Context, svc *secretkeys.Service, keyID string) error {
	rec, err := svc.Registry().Activate(ctx, keyID)
	if err != nil {
		return fmt.Errorf("controlplane keys activate: %w", err)
	}
	fmt.Fprintf(os.Stdout, "active key: %s (version %s)\n", rec.ID, rec.ProviderRef)
	fmt.Fprintln(os.Stdout, "previous active key retired; run `controlplane keys rewrap` until it reports zero, then delete the retired key once nothing references it")
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
	fmt.Fprintln(os.Stdout, "only now remove its version from the keyring files (and keep keyring backups until the database backup that referenced it expires)")
	return nil
}

func keysCheck(ctx context.Context, svc *secretkeys.Service, onlyVersion string) error {
	if version := strings.TrimSpace(onlyVersion); version != "" {
		checker, ok := svc.Provider().(secretkeys.MaterialChecker)
		if !ok {
			return fmt.Errorf("controlplane keys check: provider %s cannot report local material", svc.ProviderName())
		}
		present, err := checker.HasKeyMaterial(ctx, version)
		if err != nil {
			return fmt.Errorf("controlplane keys check: %w", err)
		}
		if !present {
			return fmt.Errorf("controlplane keys check: version %s is not provisioned on this replica", version)
		}
		fmt.Fprintf(os.Stdout, "version %s is provisioned on this replica\n", version)
		return nil
	}
	if err := svc.Registry().VerifyLocalCoverage(ctx); err != nil {
		return fmt.Errorf("controlplane keys check: %w", err)
	}
	verified, err := svc.DEKs().VerifyAll(ctx)
	if err != nil {
		return fmt.Errorf("controlplane keys check: %w", err)
	}
	fmt.Fprintf(os.Stdout, "this replica holds every recorded key version; verified %d wrapped data-encryption key(s)\n", verified)
	return nil
}
