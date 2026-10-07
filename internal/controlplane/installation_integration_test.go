//go:build integration

package controlplane

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
)

func installationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", createTestDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func execInstallation(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitInstallationBootstrapIsRepeatableAndStartupOnlyValidates(t *testing.T) {
	db := installationTestDB(t)
	ctx := context.Background()
	handle := &database{db: db}
	if err := handle.validateSchema(ctx); err == nil {
		t.Fatal("production startup initialized an empty database")
	}
	keyring := filepath.Join(t.TempDir(), "keys.json")
	if err := BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Provider().Close()
	before, err := signkeys.New(db, keys.Registry()).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	after, err := signkeys.New(db, keys.Registry()).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(signkeys.AllScopes()) || len(after) != len(before) {
		t.Fatal("platform scopes missing or duplicated")
	}
	for n := range before {
		if before[n].ID != after[n].ID {
			t.Fatal("bootstrap replaced an existing signing key")
		}
	}
	if err := handle.validateSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestBootstrapRefusesPopulatedUnversionedDatabase(t *testing.T) {
	db := installationTestDB(t)
	execInstallation(t, db, `CREATE TABLE retained_data(value TEXT)`)
	execInstallation(t, db, `INSERT INTO retained_data VALUES ('do not recreate')`)
	if err := BootstrapInstallation(context.Background(), db, filepath.Join(t.TempDir(), "keys.json")); err == nil || !strings.Contains(err.Error(), "populated") {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM retained_data`).Scan(&value); err != nil || value != "do not recreate" {
		t.Fatal(value, err)
	}
}
func TestSharedRevocationsAgreeAcrossIndependentReplicas(t *testing.T) {
	db := installationTestDB(t)
	if err := BootstrapInstallation(context.Background(), db, filepath.Join(t.TempDir(), "keys.json")); err != nil {
		t.Fatal(err)
	}
	a, b := identity.NewSharedCertificateRevocations(db), identity.NewSharedCertificateRevocations(db)
	cert := &x509.Certificate{SerialNumber: big.NewInt(0xabcd)}
	if err := b.Check(cert); err != nil {
		t.Fatal(err)
	}
	if err := a.Add("AB:CD"); err != nil {
		t.Fatal(err)
	}
	if err := b.Check(cert); !errors.Is(err, identity.ErrClientCertificateRevoked) {
		t.Fatal("replica did not observe shared revocation", err)
	}
	if err := b.Add("abcd", "123"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM certificate_revocations`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
func TestFlatInstallationConversionPreservesDataAndImportsRevocations(t *testing.T) {
	db := installationTestDB(t)
	ctx := context.Background()
	if err := BootstrapInstallation(ctx, db, filepath.Join(t.TempDir(), "keys.json")); err != nil {
		t.Fatal(err)
	}
	execInstallation(t, db, `DROP TABLE certificate_revocations; DROP TABLE recovery_runtime_authority; DROP TABLE recovery_network_reservations`)
	execInstallation(t, db, `UPDATE schema_migrations SET version=41`)
	execInstallation(t, db, `CREATE TABLE retained_data(value TEXT)`)
	execInstallation(t, db, `INSERT INTO retained_data VALUES ('persistent application data')`)
	execInstallation(t, db, `CREATE SCHEMA dashboard`)
	execInstallation(t, db, `CREATE TABLE dashboard.schema_migrations(version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`)
	execInstallation(t, db, `INSERT INTO dashboard.schema_migrations VALUES (1,statement_timestamp()),(2,statement_timestamp())`)
	execInstallation(t, db, `CREATE TABLE dashboard.onboarding(user_id TEXT PRIMARY KEY,builder TEXT NOT NULL)`)
	execInstallation(t, db, `INSERT INTO dashboard.onboarding VALUES ('operator','native')`)
	conversion := Conversion{FromSchema: 41, Backup: "s3://verified-complete-backup", DataLossCutoff: time.Now().UTC(), ConsoleSchema: "dashboard", RevokedSerials: []string{"0xABCD"}}
	if err := ConvertInstallationSchema(ctx, db, conversion); err != nil {
		t.Fatal(err)
	}
	if err := ConvertInstallationSchema(ctx, db, conversion); err != nil {
		t.Fatal("conversion is not repeatable", err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM retained_data`).Scan(&value); err != nil || value != "persistent application data" {
		t.Fatal(value, err)
	}
	if err := db.QueryRow(`SELECT builder FROM dashboard.onboarding WHERE user_id='operator'`).Scan(&value); err != nil || value != "native" {
		t.Fatal(value, err)
	}
	var version, count int
	if err := db.QueryRow(`SELECT MAX(version),COUNT(*) FROM dashboard.schema_migrations`).Scan(&version, &count); err != nil || version != 3 || count != 1 {
		t.Fatal(version, count, err)
	}
	if err := identity.NewSharedCertificateRevocations(db).Check(&x509.Certificate{SerialNumber: big.NewInt(0xabcd)}); !errors.Is(err, identity.ErrClientCertificateRevoked) {
		t.Fatal("revocation import lost", err)
	}
}

func TestFlatInstallationConversionFrom42PreservesSharedRevocations(t *testing.T) {
	db := installationTestDB(t)
	ctx := context.Background()
	if err := BootstrapInstallation(ctx, db, filepath.Join(t.TempDir(), "keys.json")); err != nil {
		t.Fatal(err)
	}
	execInstallation(t, db, `DROP TABLE recovery_runtime_authority; DROP TABLE recovery_network_reservations`)
	execInstallation(t, db, `UPDATE schema_migrations SET version=42`)
	execInstallation(t, db, `INSERT INTO certificate_revocations VALUES ('abcd',statement_timestamp())`)
	execInstallation(t, db, `CREATE SCHEMA dashboard; CREATE TABLE dashboard.schema_migrations(version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL); INSERT INTO dashboard.schema_migrations VALUES (3,statement_timestamp())`)
	conversion := Conversion{FromSchema: 42, Backup: "s3://verified-complete-backup", DataLossCutoff: time.Now().UTC(), ConsoleSchema: "dashboard"}
	if err := ConvertInstallationSchema(ctx, db, conversion); err != nil {
		t.Fatal(err)
	}
	if err := ConvertInstallationSchema(ctx, db, conversion); err != nil {
		t.Fatal("flat conversion cannot resume", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM certificate_revocations WHERE serial='abcd'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("conversion discarded shared revocations", count, err)
	}
	if err := (&database{db: db}).validateSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestConversionRequiresBackupAndStoppedOldRelease(t *testing.T) {
	db := installationTestDB(t)
	ctx := context.Background()
	if err := BootstrapInstallation(ctx, db, filepath.Join(t.TempDir(), "keys.json")); err != nil {
		t.Fatal(err)
	}
	execInstallation(t, db, `DROP TABLE certificate_revocations; DROP TABLE recovery_runtime_authority; DROP TABLE recovery_network_reservations`)
	execInstallation(t, db, `UPDATE schema_migrations SET version=41`)
	c := Conversion{FromSchema: 41, ConsoleSchema: "dashboard"}
	if err := ConvertInstallationSchema(ctx, db, c); err == nil {
		t.Fatal("conversion without a complete backup")
	}
	c.Backup = "verified"
	c.DataLossCutoff = time.Now().UTC()
	execInstallation(t, db, `INSERT INTO control_plane_leases(name,holder_id,fencing_token,expires_at,updated_at) VALUES ('old-release','replica',1,statement_timestamp()+INTERVAL '1 hour',statement_timestamp())`)
	if err := ConvertInstallationSchema(ctx, db, c); err == nil || !strings.Contains(err.Error(), "live leases") {
		t.Fatal("live old release allowed conversion", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 41 {
		t.Fatal("rejected conversion changed schema", version, err)
	}
}
