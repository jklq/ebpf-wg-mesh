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
	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/recovery"
	"ebof-wg-mesh/internal/testdb"
)

// checkRestoredDB opens (creating when needed) one named database on the
// test server with the platform schema selected and all three schemas made.
func checkRestoredDB(t *testing.T, ctx context.Context, server *testdb.Server, name string) *sql.DB {
	t.Helper()
	admin := testdb.NormalizeURL(server.PGURL())
	maintenance, err := platformdb.OpenURL(ctx, admin.String(), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	var exists bool
	if err := maintenance.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := maintenance.ExecContext(ctx, "CREATE DATABASE "+platformdb.QuoteIdentifier(name)); err != nil {
			t.Fatal(err)
		}
	}
	target, err := platformdb.ReplaceDatabase(admin.String(), name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := platformdb.OpenURL(ctx, target, platformdb.SchemaPlatform)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range platformdb.Schemas {
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestRestoredPlatformIntegrityIdentitiesAndDecryption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := testdb.Start("")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	db := checkRestoredDB(t, ctx, server, "platform_original")
	keyring := filepath.Join(t.TempDir(), "keyring.json")
	if err := controlplane.BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE console.schema_migrations(version INT PRIMARY KEY); INSERT INTO console.schema_migrations VALUES (3); CREATE TABLE console.accounts(user_id TEXT,provider_subject TEXT,access_token TEXT,refresh_token TEXT,provider TEXT)`); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("c", 32))
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	token := "ghe1." + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte("oauth-token"), []byte("github-oauth-token:v1\x00user\x00subject\x00access")))
	if _, err := db.ExecContext(ctx, `INSERT INTO console.accounts VALUES ('user','subject',$1,'','github')`, token); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Register(ctx, db, "production", "r42", []recovery.Requirement{{Kind: "release", ID: "r42"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := recovery.ReadSnapshot(ctx, db, "production", "console")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.WrapProbes) == 0 || len(snapshot.ConsoleProbes) != 1 || len(snapshot.Identities["platform_signing_keys"]) == 0 {
		t.Fatal("missing historical ciphertext and signing identity probes")
	}
	db.Close()

	// A template copy stands in for the pgBackRest point-in-time restore:
	// what CheckRestoredDatabase must prove is that the copied database
	// still carries the exact identities and decryptable ciphertext.
	admin := testdb.NormalizeURL(server.PGURL())
	maintenance, err := platformdb.OpenURL(ctx, admin.String(), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	if _, err := maintenance.ExecContext(ctx, `CREATE DATABASE platform_restored TEMPLATE platform_original`); err != nil {
		t.Fatal(err)
	}
	target, err := platformdb.ReplaceDatabase(admin.String(), "platform_restored")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := platformdb.OpenURL(ctx, target, platformdb.SchemaPlatform)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := recovery.CheckEmptyDestination(ctx, restored); err == nil {
		t.Fatal("occupied restore destination accepted as empty")
	}
	empty := checkRestoredDB(t, ctx, server, "platform_empty")
	defer empty.Close()
	if err := recovery.CheckEmptyDestination(ctx, empty); err != nil {
		t.Fatal("empty destination rejected:", err)
	}
	consoleKey := filepath.Join(t.TempDir(), "console.key")
	if err := os.WriteFile(consoleKey, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	point := recovery.Point{Snapshot: snapshot}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "console", keyring, []string{consoleKey}); err != nil {
		t.Fatal("restored platform validation", err)
	}
	if err := os.WriteFile(consoleKey, []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "console", keyring, []string{consoleKey}); err == nil {
		t.Fatal("wrong console key accepted after restore")
	}
	if err := os.WriteFile(keyring, []byte(`{"version":1,"keys":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recovery.CheckRestoredDatabase(ctx, restored, point, "console", keyring, []string{consoleKey}); err == nil {
		t.Fatal("missing master keys accepted after restore")
	}
}
