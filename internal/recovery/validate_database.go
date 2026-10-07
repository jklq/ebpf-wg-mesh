package recovery

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
)

func CheckRestoredDatabase(ctx context.Context, db *sql.DB, p Point, consoleSchema, keyring string, consoleKeys []string) error {
	var schema, consoleVersion int
	quote := func(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"" }
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&schema); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM `+quote(consoleSchema)+`.schema_migrations`).Scan(&consoleVersion); err != nil {
		return err
	}
	if schema != p.Snapshot.Schema || consoleVersion != p.Snapshot.ConsoleSchema {
		return fmt.Errorf("restored platform/console schemas differ from the recovery timestamp")
	}
	for table, expected := range p.Snapshot.Identities {
		column := "id"
		if table == "ingress_nodes" {
			column = "node_id"
		}
		rows, err := db.QueryContext(ctx, `SELECT `+quote(column)+` FROM `+quote(table)+` ORDER BY `+quote(column))
		if err != nil {
			return err
		}
		var actual []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			actual = append(actual, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(actual) == 0 {
			actual = []string{}
		}
		if string(jsonBytes(actual)) != string(jsonBytes(expected)) {
			return fmt.Errorf("restored platform identities differ for %s", table)
		}
	}
	provider, err := secretkeys.NewKeyring(keyring, secretkeys.KeyringOptions{})
	if err != nil {
		return err
	}
	secrets := secretkeys.New(db, provider)
	defer secrets.Close()
	if err := secrets.Registry().VerifyLocalCoverage(ctx); err != nil {
		return err
	}
	if _, err := secrets.DEKs().VerifyAll(ctx); err != nil {
		return err
	}
	signing := signkeys.New(db, secrets.Registry())
	if _, err := signing.VerifyAll(ctx); err != nil {
		return err
	}
	if err := checkConsoleTokens(ctx, db, quote(consoleSchema), consoleKeys); err != nil {
		return err
	}
	return scrubDatabases(ctx, db)
}

func scrubDatabases(ctx context.Context, db *sql.DB) error {
	quote := func(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"" }
	rows, err := db.QueryContext(ctx, `SELECT database_name FROM [SHOW DATABASES] WHERE database_name<>'system'`)
	if err != nil {
		return err
	}
	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		databases = append(databases, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var tables []string
	for _, name := range databases {
		rows, err := db.QueryContext(ctx, `SELECT table_schema,table_name FROM `+quote(name)+`.information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('pg_catalog','information_schema','crdb_internal')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var schema, table string
			if err := rows.Scan(&schema, &table); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, quote(name)+"."+quote(schema)+"."+quote(table))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, "EXPERIMENTAL SCRUB TABLE "+table)
		if err != nil {
			return fmt.Errorf("database integrity check failed: %w", err)
		}
		dirty := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("database integrity check found corruption in %s", table)
		}
	}
	return nil
}

func checkConsoleTokens(ctx context.Context, db *sql.DB, schema string, files []string) error {
	var keys [][]byte
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return fmt.Errorf("invalid restored console token key")
		}
		keys = append(keys, key)
		defer clear(key)
	}
	if len(keys) == 0 {
		return fmt.Errorf("restored console token encryption keys are missing")
	}
	rows, err := db.QueryContext(ctx, `SELECT user_id,provider_subject,access_token,refresh_token FROM `+schema+`.accounts WHERE provider='github'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var user, subject, access, refresh string
		if err := rows.Scan(&user, &subject, &access, &refresh); err != nil {
			return err
		}
		for kind, token := range map[string]string{"access": access, "refresh": refresh} {
			if token == "" {
				continue
			}
			if !strings.HasPrefix(token, "ghe1.") {
				return fmt.Errorf("restored console token has an invalid encryption format")
			}
			payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "ghe1."))
			if err != nil || len(payload) <= 28 {
				return fmt.Errorf("restored console token is invalid")
			}
			opened := false
			for _, key := range keys {
				block, _ := aes.NewCipher(key)
				aead, _ := cipher.NewGCM(block)
				plaintext, err := aead.Open(nil, payload[:12], payload[12:], []byte("github-oauth-token:v1\x00"+user+"\x00"+subject+"\x00"+kind))
				if err == nil {
					clear(plaintext)
					opened = true
					break
				}
			}
			if !opened {
				return fmt.Errorf("restored console token cannot be decrypted")
			}
		}
	}
	return rows.Err()
}
