//go:build integration

package recovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/testdb"
)

// The recovery inventory is read live from PostgreSQL in one repeatable-read
// snapshot bound to a WAL segment; a later mutation must not leak into it,
// and the segment must be archivable before a point can be published.
func TestSnapshotBindsInventoryToWALSegment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := testdb.Start("")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	admin := testdb.NormalizeURL(server.PGURL())
	maintenance, err := platformdb.OpenURL(ctx, admin.String(), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	if _, err := maintenance.ExecContext(ctx, `CREATE DATABASE platform_snapshot_test`); err != nil {
		t.Fatal(err)
	}
	target, err := platformdb.ReplaceDatabase(admin.String(), "platform_snapshot_test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := platformdb.OpenURL(ctx, target, platformdb.SchemaPlatform)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, schema := range platformdb.Schemas {
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`CREATE TABLE platform.schema_migrations(version INT PRIMARY KEY); INSERT INTO platform.schema_migrations VALUES (42)`,
		`CREATE TABLE console.schema_migrations(version INT PRIMARY KEY); INSERT INTO console.schema_migrations VALUES (3)`,
		`CREATE TABLE platform.envelope_keys(id TEXT DEFAULT 'k' PRIMARY KEY,provider_ref TEXT); INSERT INTO platform.envelope_keys(provider_ref) VALUES ('k1')`,
		`CREATE TABLE platform.envelope_data_keys(id TEXT,wrapping_key_id TEXT,wrapped_dek BYTEA)`,
		`CREATE TABLE console.accounts(user_id TEXT,provider_subject TEXT,access_token TEXT,refresh_token TEXT,provider TEXT)`,
		`CREATE TABLE platform.source_snapshots(digest TEXT,object_key TEXT); INSERT INTO platform.source_snapshots VALUES ('sha256:abc','archive-original')`,
		`CREATE TABLE platform.source_archive_objects(digest TEXT,object_key TEXT)`,
		`CREATE TABLE platform.build_runs(source_snapshot_digest TEXT,state TEXT)`,
		`CREATE TABLE platform.build_artifacts(kind TEXT,image_ref TEXT,image_manifest_digest TEXT,image_retained BOOL); INSERT INTO platform.build_artifacts VALUES ('direct_image','external.example/a@sha256:123','sha256:123',true)`,
		`CREATE TABLE platform.agent_registrations(id TEXT PRIMARY KEY); INSERT INTO platform.agent_registrations VALUES ('agent-original')`,
		`CREATE TABLE platform.ingress_nodes(node_id TEXT PRIMARY KEY); INSERT INTO platform.ingress_nodes VALUES ('ingress-original')`,
		`CREATE TABLE platform.platform_signing_keys(id TEXT PRIMARY KEY,wrapping_key_id TEXT,wrapped_key BYTEA); INSERT INTO platform.platform_signing_keys(id) VALUES ('signing-original')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err := Register(ctx, db, "installation", "release1", []Requirement{{Kind: "release", ID: "release1"}}); err != nil {
		t.Fatal(err)
	}

	snapshot, walSegment, err := ReadSnapshot(ctx, db, "installation", "console")
	if err != nil {
		t.Fatal(err)
	}
	// Mutations after the snapshot must not appear in it (repeatable read),
	// and must appear in a second snapshot (the first was not stale cache).
	if _, err := db.ExecContext(ctx, `DELETE FROM source_snapshots; UPDATE agent_registrations SET id='agent-new'`); err != nil {
		t.Fatal(err)
	}
	if snapshot.Schema != 42 || snapshot.ConsoleSchema != 3 || len(snapshot.ExternalImages) != 1 ||
		len(snapshot.Identities["agent_registrations"]) != 1 || snapshot.Identities["agent_registrations"][0] != "agent-original" {
		t.Fatal("snapshot missed the platform inventory", snapshot)
	}
	foundSource := false
	for _, r := range snapshot.Requirements {
		if r.Kind == "source" && r.ID == "archive-original" {
			foundSource = true
		}
	}
	if !foundSource {
		t.Fatal("snapshot lost the original source archive", snapshot.Requirements)
	}
	if timeline, _, err := platformdb.ParseWALSegment(walSegment); err != nil || timeline != 1 {
		t.Fatal("snapshot WAL segment", walSegment, err)
	}
	if err := EnsureWALArchived(ctx, db, walSegment); err != nil {
		t.Fatal("snapshot segment was not archived by the test archiver:", err)
	}
	// The test archiver is not pgBackRest; verification must fail closed.
	if err := VerifyArchiving(ctx, db, "platform"); err == nil || !strings.Contains(err.Error(), "pgBackRest") {
		t.Fatal("non-pgBackRest archive command accepted:", err)
	}
	second, _, err := ReadSnapshot(ctx, db, "installation", "console")
	if err != nil {
		t.Fatal(err)
	}
	if second.Identities["agent_registrations"][0] != "agent-new" {
		t.Fatal("second snapshot did not observe committed mutations")
	}
}

type fakeBackups struct{ info platformdb.RepoInfo }

func (f fakeBackups) CreateStanza(context.Context) error                  { return nil }
func (f fakeBackups) Check(context.Context) error                         { return nil }
func (f fakeBackups) Backup(context.Context, platformdb.BackupType) error { return nil }
func (f fakeBackups) Info(context.Context) (platformdb.RepoInfo, error)   { return f.info, nil }
func (f fakeBackups) Restore(context.Context, string, string, time.Time) error {
	return nil
}
func (f fakeBackups) Expire(context.Context) error { return nil }

func TestInspectRepositorySelectsChainCoveringSnapshot(t *testing.T) {
	stop := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	info := platformdb.RepoInfo{Name: "platform",
		Backups: []platformdb.BackupInfo{
			{Label: "F1", Type: platformdb.BackupFull, Timeline: 1, WALStart: "000000010000000000000001", WALStop: "000000010000000000000003", StopTime: stop.Add(-time.Hour)},
			{Label: "I1", Type: platformdb.BackupIncremental, Prior: "F1", Timeline: 1, WALStart: "000000010000000000000004", WALStop: "000000010000000000000006", StopTime: stop},
		},
		Archives: []platformdb.ArchiveRange{{Timeline: 1, Min: "000000010000000000000001", Max: "000000010000000000000009"}},
	}
	at := stop.Add(time.Second)
	b, err := InspectRepository(context.Background(), fakeBackups{info}, "platform", "s3://bucket/production/database", "", at, "000000010000000000000007")
	if err != nil {
		t.Fatal(err)
	}
	if b.BackupLabel != "I1" || len(b.Chain) != 2 || b.WALStart != "000000010000000000000001" || b.WALStop != "000000010000000000000007" {
		t.Fatal("selected chain", b)
	}
	if _, err := InspectRepository(context.Background(), fakeBackups{info}, "platform", "s3://bucket/production/database", "", at, "00000001000000000000000A"); err == nil {
		t.Fatal("snapshot beyond archived WAL accepted")
	}
}
