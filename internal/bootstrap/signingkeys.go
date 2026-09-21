package bootstrap

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// signkeysSchemaVersion is the minimum control-plane schema version carrying
// the platform signing-key tables. The operator CLI refuses to run against
// older databases instead of failing mid-rotation.
const signkeysSchemaVersion = 26

// RunSigningKeys implements `controlplane signing-keys`, the operator
// command surface for platform signing-key lifecycle:
//
//	init --scope S [--scope S...] [--hmac-secret V | --hmac-secret-file F]
//	                           create each scope's first active key
//	list                   show scopes, kids, states, and wrapping keys
//	rotate-start --scope S [--hmac-secret V | --hmac-secret-file F]
//	                           demote the active key to retiring and activate a fresh key
//	rotate-finish --scope S [--min-overlap D] [--force]
//	                           delete the retiring key once its overlap elapsed
//	export --scope S [--out PATH]
//	                           write the trust bundle (ECDSA scopes) or active
//	                           secret (HMAC scopes) for dashboard/registry provisioning
//	check                  verify every signing key unwraps
//	issue-client-cert --caller-class C --caller-id ID [--ttl D]
//	                           mint a fresh mTLS client identity from the active CA
//
// Rotation is overlap, not ceremony: rotate-start, provision the new
// material to verifiers that hold it (dashboard secrets, registry
// rootcertbundle), wait out the scope's overlap, then rotate-finish. The
// lifetime table and runbook live in docs/signing-keys.md.
func RunSigningKeys(args []string) error {
	if len(args) == 0 {
		return signingKeysUsageError()
	}
	command, rest := args[0], args[1:]
	switch command {
	case "init", "list", "rotate-start", "rotate-finish", "export", "check", "issue-client-cert":
		return runSigningKeysCommand(command, rest)
	default:
		return signingKeysUsageError()
	}
}

func signingKeysUsageError() error {
	return fmt.Errorf("usage: controlplane signing-keys <init|list|rotate-start|rotate-finish|export|check|issue-client-cert> [flags]")
}

func runSigningKeysCommand(command string, args []string) error {
	var dbURL, stateDir, hmacSecret, hmacSecretFile, registryIssuer, minOverlapRaw, outPath string
	var scopes scopeListFlag
	var callerClass, callerID, ttlRaw string
	var force bool
	var keysCfg config.SecretKeysConfig
	fs := flag.NewFlagSet("controlplane signing-keys "+command, flag.ContinueOnError)
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &stateDir, "state-dir", "CONTROLPLANE_STATE_DIR", "var/controlplane", "")
	stringFlag(fs, &keysCfg.KeyringPath, "secret-keys-keyring", "CONTROLPLANE_SECRET_KEYS_KEYRING", "", "provisioned master-key ring file")
	switch command {
	case "init", "rotate-start", "rotate-finish", "export":
		fs.Var(&scopes, "scope", "signing scope (repeatable for init): "+strings.Join(signkeys.AllScopes(), ", "))
	}
	switch command {
	case "init", "rotate-start":
		stringFlag(fs, &hmacSecret, "hmac-secret", "CONTROLPLANE_SIGNING_KEYS_HMAC_SECRET", "", "operator-supplied HMAC secret for user-assertion/dashboard-session scopes; generated when empty")
		stringFlag(fs, &hmacSecretFile, "hmac-secret-file", "CONTROLPLANE_SIGNING_KEYS_HMAC_SECRET_FILE", "", "")
	}
	if command == "rotate-start" {
		stringFlag(fs, &registryIssuer, "registry-issuer", "CONTROLPLANE_SIGNING_KEYS_REGISTRY_ISSUER", "", "issuer embedded in a generated registry signer certificate")
	}
	if command == "rotate-finish" {
		stringFlag(fs, &minOverlapRaw, "min-overlap", "CONTROLPLANE_SIGNING_KEYS_MIN_OVERLAP", "", "extra overlap required before deleting the retiring key, e.g. 72h (floored at the scope default)")
		fs.BoolVar(&force, "force", false, "delete the retiring key before its overlap elapsed (tests and documented emergencies only)")
	}
	if command == "export" {
		stringFlag(fs, &outPath, "out", "CONTROLPLANE_SIGNING_KEYS_EXPORT_OUT", "", "write to this file instead of stdout")
	}
	if command == "issue-client-cert" {
		stringFlag(fs, &callerClass, "caller-class", "CONTROLPLANE_SIGNING_KEYS_CALLER_CLASS", "", "client caller class (dashboard or builder)")
		stringFlag(fs, &callerID, "caller-id", "CONTROLPLANE_SIGNING_KEYS_CALLER_ID", "", "client caller id")
		stringFlag(fs, &ttlRaw, "ttl", "CONTROLPLANE_SIGNING_KEYS_CLIENT_TTL", "24h", "client certificate lifetime")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(keysCfg.KeyringPath) == "" {
		keysCfg.KeyringPath = filepath.Join(stateDir, "secret-keys", "keys.json")
	}
	if strings.TrimSpace(dbURL) == "" {
		return fmt.Errorf("controlplane signing-keys: db-url is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("controlplane signing-keys: open database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("controlplane signing-keys: ping database: %w", err)
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("controlplane signing-keys: read schema version: %w", err)
	}
	if version < signkeysSchemaVersion {
		return fmt.Errorf("controlplane signing-keys: database schema v%d is older than signing keys v%d; start the control plane first",
			version, signkeysSchemaVersion)
	}
	// The CLI never auto-generates envelope state: init is the explicit
	// command for that, and every other command fails closed on missing
	// material.
	provider, err := secretkeys.OpenProvider(keysCfg, secretkeys.Options{})
	if err != nil {
		return fmt.Errorf("controlplane signing-keys: %w", err)
	}
	defer provider.Close()
	svc := signkeys.New(db, secretkeys.NewRegistry(db, provider))

	switch command {
	case "init":
		return signingKeysInit(ctx, svc, scopes, hmacSecret, hmacSecretFile)
	case "list":
		return signingKeysList(ctx, svc)
	case "rotate-start":
		return signingKeysRotateStart(ctx, svc, scopes, hmacSecret, hmacSecretFile, registryIssuer)
	case "rotate-finish":
		return signingKeysRotateFinish(ctx, svc, scopes, minOverlapRaw, force)
	case "export":
		return signingKeysExport(ctx, svc, scopes, outPath)
	case "check":
		return signingKeysCheck(ctx, svc)
	case "issue-client-cert":
		return signingKeysIssueClientCert(ctx, svc, callerClass, callerID, ttlRaw)
	default:
		return signingKeysUsageError()
	}
}

type scopeListFlag []string

func (f *scopeListFlag) String() string { return strings.Join(*f, ",") }

func (f *scopeListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func signingKeysInit(ctx context.Context, svc *signkeys.Service, scopes []string, hmacSecret, hmacSecretFile string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("controlplane signing-keys init: at least one --scope is required (%s)", strings.Join(signkeys.AllScopes(), ", "))
	}
	secret, err := resolveHMACSecret(scopes, hmacSecret, hmacSecretFile)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		rec, err := svc.Init(ctx, scope, signkeys.RotateOptions{HMACSecret: secretForScope(scope, secret)})
		if err != nil {
			return fmt.Errorf("controlplane signing-keys init: %w", err)
		}
		fmt.Fprintf(os.Stdout, "initialized scope %s: active key %s (kid %s)\n", rec.Scope, rec.ID, rec.KID)
	}
	return nil
}

func signingKeysList(ctx context.Context, svc *signkeys.Service) error {
	records, err := svc.List(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stdout, "no signing keys; initialize each scope with `controlplane signing-keys init --scope <scope>`")
		return nil
	}
	fmt.Fprintln(os.Stdout, "SCOPE\tSTATE\tKID\tID\tTYPE\tWRAPPING KEY\tRETIRED AT")
	for _, rec := range records {
		retired := "-"
		if rec.RetiredAt != nil {
			retired = rec.RetiredAt.Format(time.RFC3339)
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rec.Scope, rec.State, rec.KID, rec.ID, rec.KeyType, rec.WrappingKeyID, retired)
	}
	return nil
}

func signingKeysRotateStart(ctx context.Context, svc *signkeys.Service, scopes []string, hmacSecret, hmacSecretFile, registryIssuer string) error {
	if len(scopes) != 1 {
		return fmt.Errorf("controlplane signing-keys rotate-start: exactly one --scope is required")
	}
	secret, err := resolveHMACSecret(scopes, hmacSecret, hmacSecretFile)
	if err != nil {
		return err
	}
	active, retiring, err := svc.RotateStart(ctx, scopes[0], signkeys.RotateOptions{
		HMACSecret:     secretForScope(scopes[0], secret),
		RegistryIssuer: registryIssuer,
	})
	if err != nil {
		return fmt.Errorf("controlplane signing-keys rotate-start: %w", err)
	}
	minimum, _ := signkeys.MinOverlapForScope(scopes[0])
	fmt.Fprintf(os.Stdout, "scope %s: active key %s (kid %s), retiring key %s\n", active.Scope, active.ID, active.KID, retiring.ID)
	fmt.Fprintf(os.Stdout, "both keys verify now; run `controlplane signing-keys rotate-finish --scope %s` once %s of overlap elapsed (see docs/signing-keys.md)\n", scopes[0], minimum)
	return nil
}

func signingKeysRotateFinish(ctx context.Context, svc *signkeys.Service, scopes []string, minOverlapRaw string, force bool) error {
	if len(scopes) != 1 {
		return fmt.Errorf("controlplane signing-keys rotate-finish: exactly one --scope is required")
	}
	var minOverlap time.Duration
	if trimmed := strings.TrimSpace(minOverlapRaw); trimmed != "" {
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			return fmt.Errorf("controlplane signing-keys rotate-finish: invalid --min-overlap %q: %w", minOverlapRaw, err)
		}
		minOverlap = parsed
	}
	rec, err := svc.RotateFinish(ctx, scopes[0], signkeys.FinishOptions{MinOverlap: minOverlap, Force: force})
	if err != nil {
		return fmt.Errorf("controlplane signing-keys rotate-finish: %w", err)
	}
	fmt.Fprintf(os.Stdout, "scope %s: deleted retiring key %s; active key continues alone\n", rec.Scope, rec.ID)
	return nil
}

func signingKeysExport(ctx context.Context, svc *signkeys.Service, scopes []string, outPath string) error {
	if len(scopes) != 1 {
		return fmt.Errorf("controlplane signing-keys export: exactly one --scope is required")
	}
	scope := scopes[0]
	keyType, err := signkeys.KeyTypeForScope(scope)
	if err != nil {
		return fmt.Errorf("controlplane signing-keys export: %w", err)
	}
	var material []byte
	switch keyType {
	case signkeys.KeyTypeECDSAP256:
		material, err = svc.PublicBundle(ctx, scope)
		if err != nil {
			return fmt.Errorf("controlplane signing-keys export: %w", err)
		}
	case signkeys.KeyTypeHMAC256:
		material, err = svc.ActiveSecret(ctx, scope)
		if err != nil {
			return fmt.Errorf("controlplane signing-keys export: %w", err)
		}
	default:
		return fmt.Errorf("controlplane signing-keys export: scope %q has unknown key type %q", scope, keyType)
	}
	if trimmed := strings.TrimSpace(outPath); trimmed != "" {
		mode := os.FileMode(0o644)
		if keyType == signkeys.KeyTypeHMAC256 {
			mode = 0o600
		}
		if err := os.WriteFile(trimmed, material, mode); err != nil {
			return fmt.Errorf("controlplane signing-keys export: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s %s to %s\n", scope, exportKind(keyType), trimmed)
		return nil
	}
	// Raw bytes, no framing: callers capture stdout byte-for-byte into
	// dashboard secret files or registry rootcertbundles.
	if _, err := os.Stdout.Write(material); err != nil {
		return fmt.Errorf("controlplane signing-keys export: %w", err)
	}
	return nil
}

func exportKind(keyType string) string {
	if keyType == signkeys.KeyTypeHMAC256 {
		return "secret"
	}
	return "trust bundle"
}

func signingKeysCheck(ctx context.Context, svc *signkeys.Service) error {
	verified, err := svc.VerifyAll(ctx)
	if err != nil {
		return fmt.Errorf("controlplane signing-keys check: %w", err)
	}
	records, err := svc.List(ctx)
	if err != nil {
		return fmt.Errorf("controlplane signing-keys check: %w", err)
	}
	active := map[string]bool{}
	var retiring []string
	for _, rec := range records {
		if rec.State == signkeys.KeyStateActive {
			active[rec.Scope] = true
		} else {
			retiring = append(retiring, rec.Scope)
		}
	}
	var missing []string
	for _, scope := range signkeys.AllScopes() {
		if !active[scope] {
			missing = append(missing, scope)
		}
	}
	fmt.Fprintf(os.Stdout, "verified %d signing key(s) unwrap on this replica\n", verified)
	if len(missing) > 0 {
		fmt.Fprintf(os.Stdout, "scopes without an active key: %s\n", strings.Join(missing, ", "))
	}
	if len(retiring) > 0 {
		fmt.Fprintf(os.Stdout, "rotations in progress: %s\n", strings.Join(retiring, ", "))
	}
	return nil
}

func signingKeysIssueClientCert(ctx context.Context, svc *signkeys.Service, callerClass, callerID, ttlRaw string) error {
	var class identity.CallerClass
	switch strings.TrimSpace(callerClass) {
	case string(identity.CallerDashboard):
		class = identity.CallerDashboard
	case string(identity.CallerBuilder):
		class = identity.CallerBuilder
	default:
		return fmt.Errorf("controlplane signing-keys issue-client-cert: caller class must be dashboard or builder")
	}
	if strings.TrimSpace(callerID) == "" {
		return fmt.Errorf("controlplane signing-keys issue-client-cert: --caller-id is required")
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(ttlRaw))
	if err != nil || ttl <= 0 {
		return fmt.Errorf("controlplane signing-keys issue-client-cert: invalid --ttl %q", ttlRaw)
	}
	material, err := identity.IssueClientCertificate(ctx, svc, class, strings.TrimSpace(callerID), ttl)
	if err != nil {
		return fmt.Errorf("controlplane signing-keys issue-client-cert: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{
		"ca_pem_b64":   base64.StdEncoding.EncodeToString(material.CAPEM),
		"cert_pem_b64": base64.StdEncoding.EncodeToString(material.CertPEM),
		"key_pem_b64":  base64.StdEncoding.EncodeToString(material.KeyPEM),
	})
}

// resolveHMACSecret loads an operator-supplied HMAC secret once for init and
// rotate-start. It is only valid for exactly one HMAC scope: ECDSA scopes
// always generate, and one secret must never seed two scopes.
func resolveHMACSecret(scopes []string, hmacSecret, hmacSecretFile string) ([]byte, error) {
	if hmacSecret != "" && hmacSecretFile != "" {
		return nil, fmt.Errorf("only one of hmac secret or hmac secret file may be configured")
	}
	raw := hmacSecret
	if hmacSecretFile != "" {
		secret, err := os.ReadFile(hmacSecretFile)
		if err != nil {
			return nil, fmt.Errorf("read hmac secret %s: %w", hmacSecretFile, err)
		}
		raw = strings.TrimSpace(string(secret))
	}
	if raw == "" {
		return nil, nil
	}
	if len(scopes) != 1 {
		return nil, fmt.Errorf("hmac secret requires exactly one --scope")
	}
	keyType, err := signkeys.KeyTypeForScope(scopes[0])
	if err != nil {
		return nil, err
	}
	if keyType != signkeys.KeyTypeHMAC256 {
		return nil, fmt.Errorf("hmac secret applies to HMAC scopes only; scope %q generates its own key", scopes[0])
	}
	return []byte(raw), nil
}

func secretForScope(scope string, secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	keyType, err := signkeys.KeyTypeForScope(scope)
	if err != nil || keyType != signkeys.KeyTypeHMAC256 {
		return nil
	}
	return secret
}
