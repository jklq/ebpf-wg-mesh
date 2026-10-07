//go:build integration

package recovery_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/recovery"
	"ebof-wg-mesh/internal/testdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

func restoreTestDB(t *testing.T, dir string) (*sql.DB, func()) {
	t.Helper()
	server, err := testserver.NewTestServer(testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", server.PGURL().String())
	if err != nil {
		server.Stop()
		t.Fatal(err)
	}
	return db, func() { db.Close(); server.Stop() }
}

func TestRestoredPlatformIntegrityIdentitiesAndDecryption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	db, stop := restoreTestDB(t, dir)
	defer stop()
	keyring := filepath.Join(t.TempDir(), "keyring.json")
	if err := controlplane.BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA dashboard; CREATE TABLE dashboard.schema_migrations(version INT PRIMARY KEY); INSERT INTO dashboard.schema_migrations VALUES (3); CREATE TABLE dashboard.accounts(user_id STRING,provider_subject STRING,access_token STRING,refresh_token STRING,provider STRING)`); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("c", 32))
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	token := "ghe1." + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte("oauth-token"), []byte("github-oauth-token:v1\x00user\x00subject\x00access")))
	if _, err := db.ExecContext(ctx, `INSERT INTO dashboard.accounts VALUES ('user','subject',$1,'','github')`, token); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Register(ctx, db, "production", "r42", []recovery.Requirement{{Kind: "release", ID: "r42"}}); err != nil {
		t.Fatal(err)
	}
	var timestamp time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recovery.ReadSnapshot(ctx, db, "production", "dashboard", timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.WrapProbes) == 0 || len(snapshot.ConsoleProbes) != 1 || len(snapshot.Identities["platform_signing_keys"]) == 0 {
		t.Fatal("missing historical ciphertext and signing identity probes")
	}
	if _, err := db.ExecContext(ctx, `BACKUP INTO 'nodelocal://1/platform' WITH revision_history`); err != nil {
		t.Fatal(err)
	}
	restored, stopRestored := restoreTestDB(t, dir)
	defer stopRestored()
	if _, err := restored.ExecContext(ctx, `RESTORE FROM LATEST IN 'nodelocal://1/platform'`); err != nil {
		t.Fatal(err)
	}
	consoleKey := filepath.Join(t.TempDir(), "console.key")
	if err := os.WriteFile(consoleKey, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	point := recovery.Point{Snapshot: snapshot}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "dashboard", keyring, []string{consoleKey}); err != nil {
		t.Fatal("restored platform validation", err)
	}
	if err := os.WriteFile(consoleKey, []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "dashboard", keyring, []string{consoleKey}); err == nil {
		t.Fatal("wrong console key accepted after native restore")
	}
	if err := os.WriteFile(keyring, []byte(`{"version":1,"keys":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "dashboard", keyring, []string{consoleKey}); err == nil {
		t.Fatal("missing master keys accepted after native restore")
	}
}
