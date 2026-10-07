package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
)

// BootstrapInstallation is an explicit administrative operation. Startup never
// creates master or platform keys in production. Repeating bootstrap preserves
// existing keys and refuses a populated unversioned or mismatched database.
func BootstrapInstallation(ctx context.Context, db *sql.DB, keyring string) error {
	handle := &database{db: db}
	if err := handle.migrate(ctx); err != nil {
		return err
	}
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{AllowGenerate: true})
	if err != nil {
		return err
	}
	defer keys.Provider().Close()
	signing := signkeys.New(db, keys.Registry())
	for _, scope := range signkeys.AllScopes() {
		if _, err := signing.EnsureActiveKey(ctx, scope, signkeys.EnsureOptions{}); err != nil {
			return err
		}
	}
	_, err = signing.VerifyAll(ctx)
	return err
}

// ConvertInstallationSchema is a direct v41 -> v42 conversion preserving all
// application tables. It is never called by startup. The controller has already
// quiesced, backed up and stopped every old component before invoking it.
type Conversion struct {
	FromSchema     int
	Backup         string
	DataLossCutoff time.Time
	ConsoleSchema  string
	RevokedSerials []string
}

func ConvertInstallationSchema(ctx context.Context, db *sql.DB, conversion Conversion) error {
	if conversion.FromSchema != 41 || conversion.Backup == "" || conversion.DataLossCutoff.IsZero() || !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(conversion.ConsoleSchema) {
		return fmt.Errorf("conversion requires source schema 41 and a complete backup with data-loss cutoff")
	}
	serials := make([]string, len(conversion.RevokedSerials))
	for n, value := range conversion.RevokedSerials {
		serial, err := identity.NormalizeCertificateSerial(value)
		if err != nil {
			return err
		}
		serials[n] = serial
	}
	handle := &database{db: db}
	err := handle.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var version, count int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0),COUNT(*) FROM schema_migrations`).Scan(&version, &count); err != nil {
			return err
		}
		if count != 1 || (version != conversion.FromSchema && version != currentSchemaVersion) {
			return fmt.Errorf("cannot convert schema %d with %d version records", version, count)
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM control_plane_leases WHERE expires_at > statement_timestamp())`).Scan(&active); err != nil {
			return err
		}
		if active {
			return errors.New("old release still owns live leases; stop all replicas and wait for their leases to expire")
		}
		if version != currentSchemaVersion {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE certificate_revocations (serial TEXT PRIMARY KEY, revoked_at TIMESTAMPTZ NOT NULL)`); err != nil {
				return err
			}
		}
		for _, serial := range serials {
			if _, err := tx.ExecContext(ctx, `INSERT INTO certificate_revocations(serial,revoked_at) VALUES ($1,statement_timestamp()) ON CONFLICT(serial) DO NOTHING`, serial); err != nil {
				return err
			}
		}
		if err := convertConsoleSchema(ctx, tx, conversion.ConsoleSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES ($1,statement_timestamp())`, currentSchemaVersion)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

func convertConsoleSchema(ctx context.Context, tx *sql.Tx, schema string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='schema_migrations')`, schema).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var maxVersion, count int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(version),0),COUNT(*) FROM %s.schema_migrations`, schema)).Scan(&maxVersion, &count); err != nil {
		return err
	}
	if maxVersion == 3 && count == 1 {
		return nil
	}
	if !(maxVersion == 1 && count == 1 || maxVersion == 2 && count == 2) {
		return fmt.Errorf("console schema %s has unsupported source versions", schema)
	}
	if maxVersion == 1 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.onboarding ADD COLUMN builder TEXT NOT NULL DEFAULT ''`, schema)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s.schema_migrations`, schema)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s.schema_migrations(version,applied_at) VALUES (3,statement_timestamp())`, schema))
	return err
}
