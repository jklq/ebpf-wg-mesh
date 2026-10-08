package recovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ScheduleSQL is a full-cluster schedule, including console SQL and the
// recovery inventory. External connections keep credentials out of SQL output.
func ScheduleSQL(installation, connection string) (string, error) {
	if installation == "" || !strings.HasPrefix(connection, "external://") || strings.ContainsAny(connection, "?\n\r") {
		return "", fmt.Errorf("native backup schedules require an installation and credential-free external connection")
	}
	return "CREATE SCHEDULE IF NOT EXISTS " + literal("recovery-"+installation) + " FOR BACKUP INTO " + literal(connection) + " WITH revision_history RECURRING " + literal(IncrementalCron) + " FULL BACKUP " + literal(FullCron) + " WITH SCHEDULE OPTIONS first_run = 'now', on_execution_failure = 'retry', on_previous_running = 'wait'", nil
}

func VerifySchedules(ctx context.Context, db *sql.DB, installation, connection string) error {
	var enabled bool
	if err := db.QueryRowContext(ctx, "SHOW CLUSTER SETTING jobs.scheduler.enabled").Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("native backup scheduler is disabled")
	}

	rows, err := db.QueryContext(ctx, `SELECT recurrence,command,schedule_status,COALESCE(state,'') FROM [SHOW SCHEDULES] WHERE label=$1`, "recovery-"+installation)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var cron, command, status, state string
		if err := rows.Scan(&cron, &command, &status, &state); err != nil {
			return err
		}
		if (cron != FullCron && cron != IncrementalCron) || !strings.Contains(command, literal(connection)) || !strings.Contains(command, "revision_history") {
			return fmt.Errorf("native recovery backup schedule has changed its cadence, destination, or revision history")
		}
		if seen[cron] {
			return fmt.Errorf("duplicate native recovery backup schedules")
		}
		if status != "ACTIVE" && !(cron == IncrementalCron && strings.Contains(state, "Waiting for initial backup")) {
			return fmt.Errorf("native backup schedule is paused or failed: %s", state)
		}
		seen[cron] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != 2 {
		return fmt.Errorf("full and incremental native backup schedules are required")
	}
	return nil
}

// Read only the supported SHOW CREATE response. Credential-bearing URI values
// are used in memory to bind native backups to the independently verified store;
// they never enter catalog manifests or diagnostics.
func externalConnectionURI(ctx context.Context, db *sql.DB, connection string) (*url.URL, error) {
	u, err := url.Parse(connection)
	if err != nil || u.Scheme != "external" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid external recovery connection identity")
	}
	var name, statement string
	identifier := "\"" + strings.ReplaceAll(u.Host, "\"", "\"\"") + "\""
	if err := db.QueryRowContext(ctx, "SHOW CREATE EXTERNAL CONNECTION "+identifier).Scan(&name, &statement); err != nil {
		return nil, fmt.Errorf("read native recovery connection: %w", err)
	}
	marker := " AS '"
	start := strings.Index(statement, marker)
	if name != u.Host || start < 0 || !strings.HasSuffix(statement, "'") {
		return nil, fmt.Errorf("unexpected native external connection definition")
	}
	raw := strings.ReplaceAll(statement[start+len(marker):len(statement)-1], "''", "'")
	location, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("native recovery connection URI is invalid")
	}
	return location, nil
}

func VerifyBackupDestination(ctx context.Context, db *sql.DB, connection string, storage StorageConfig, prefix string) error {
	u, err := externalConnectionURI(ctx, db, connection)
	if err != nil {
		return err
	}
	return checkBackupDestination(u, storage, prefix)
}

func checkBackupDestination(u *url.URL, storage StorageConfig, prefix string) error {
	if u.Scheme != "s3" || u.Host != storage.Bucket || u.User != nil || strings.Trim(u.Path, "/") != prefix || !strings.HasPrefix(prefix, storage.Prefix+"/") || strings.TrimRight(u.Query().Get("AWS_ENDPOINT"), "/") != strings.TrimRight(storage.Endpoint, "/") {
		return fmt.Errorf("native backup bucket, prefix or endpoint differs from independent recovery storage")
	}
	return nil
}

// Register records installer-owned dependencies in SQL so historical inventory
// reads use the same timestamp as application metadata. Keep both releases
// registered during cutover; FinishRelease removes the previous release only
// after platform availability has been independently verified.
func Register(ctx context.Context, db *sql.DB, installation, release string, requirements []Requirement) error {
	if _, err := db.ExecContext(ctx, `CREATE DATABASE IF NOT EXISTS platform_recovery`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.installations (
	 installation STRING NOT NULL, release STRING NOT NULL, requirements JSONB NOT NULL,
	 PRIMARY KEY (installation, release))`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPSERT INTO platform_recovery.public.installations VALUES ($1,$2,$3)`, installation, release, string(jsonBytes(requirements)))
	return err
}
func FinishRelease(ctx context.Context, db *sql.DB, installation, release string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM platform_recovery.public.installations WHERE installation=$1 AND release<>$2`, installation, release)
	return err
}

func ReadSnapshot(ctx context.Context, db *sql.DB, installation, consoleSchema string, t time.Time) (Snapshot, error) {
	s := Snapshot{Timestamp: t, Identities: map[string][]string{}}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION AS OF SYSTEM TIME "+literal(t.UTC().Format(time.RFC3339Nano))); err != nil {
		return s, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&s.Schema); err != nil {
		return s, err
	}
	identifier := "\"" + strings.ReplaceAll(consoleSchema, "\"", "\"\"") + "\""
	if consoleSchema == "" {
		return s, fmt.Errorf("console SQL schema is required")
	}
	// Console's flat schema records exactly one version.
	if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM `+identifier+`.schema_migrations`).Scan(&s.ConsoleSchema); err != nil {
		return s, fmt.Errorf("console schema inventory: %w", err)
	}
	query := func(statement string, visit func(*sql.Rows) error) error {
		rows, err := tx.QueryContext(ctx, statement)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := visit(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := query(`SELECT requirements FROM platform_recovery.public.installations WHERE installation=`+literal(installation), func(rows *sql.Rows) error {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return err
		}
		var needs []Requirement
		if err := json.Unmarshal(b, &needs); err != nil {
			return err
		}
		for _, need := range needs {
			if need.Kind == "external-image" {
				s.ExternalImages = append(s.ExternalImages, need.ID)
			} else {
				s.Requirements = append(s.Requirements, need)
			}
		}
		return nil
	}); err != nil {
		return s, err
	}
	if err := query(`SELECT provider_ref FROM envelope_keys`, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.Requirements = append(s.Requirements, Requirement{Kind: "keyring", ID: id})
		return nil
	}); err != nil {
		return s, err
	}
	if err := query(`SELECT DISTINCT digest, object_key FROM source_snapshots WHERE object_key<>''
	 UNION SELECT o.digest,o.object_key FROM source_archive_objects o JOIN build_runs b ON b.source_snapshot_digest=o.digest WHERE b.state IN ('queued','running')`, func(rows *sql.Rows) error {
		var digest, key string
		if err := rows.Scan(&digest, &key); err != nil {
			return err
		}
		s.Requirements = append(s.Requirements, Requirement{Kind: "source", ID: key, Digest: digest})
		return nil
	}); err != nil {
		return s, err
	}
	if err := query(`SELECT DISTINCT kind,image_ref,image_manifest_digest FROM build_artifacts WHERE image_retained=true`, func(rows *sql.Rows) error {
		var kind, ref, digest string
		if err := rows.Scan(&kind, &ref, &digest); err != nil {
			return err
		}
		if kind == "direct_image" {
			s.ExternalImages = append(s.ExternalImages, ref)
		} else {
			s.Requirements = append(s.Requirements, Requirement{Kind: "image", ID: ref, Digest: digest})
		}
		return nil
	}); err != nil {
		return s, err
	}
	for _, table := range []string{"agent_registrations", "ingress_nodes", "platform_signing_keys"} {
		column := "id"
		if table == "ingress_nodes" {
			column = "node_id"
		}
		if err := query(`SELECT `+column+` FROM `+table+` ORDER BY `+column, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			s.Identities[table] = append(s.Identities[table], id)
			return nil
		}); err != nil {
			return s, err
		}
		if s.Identities[table] == nil {
			s.Identities[table] = []string{}
		}
	}
	if err := readSecretProbes(ctx, tx, &s, identifier); err != nil {
		return s, err
	}
	unique := map[string]Requirement{}
	for _, r := range s.Requirements {
		k := identity(r)
		if prev, ok := unique[k]; ok && prev.Digest != r.Digest {
			return s, fmt.Errorf("historical dependency %s has conflicting immutable identities", k)
		}
		unique[k] = r
	}
	s.Requirements = nil
	for _, r := range unique {
		s.Requirements = append(s.Requirements, r)
	}
	sort.Slice(s.Requirements, func(i, j int) bool { return identity(s.Requirements[i]) < identity(s.Requirements[j]) })
	return s, tx.Commit()
}

// InspectBackup uses the supported SHOW BACKUP response, never internal
// protobuf/manifest JSON. Storage objects are frozen separately by version.
func InspectBackup(ctx context.Context, db *sql.DB, collection, subdirectory string) (Database, error) {
	b := Database{Collection: collection, Subdirectory: subdirectory}
	if !strings.HasPrefix(collection, "external://") || subdirectory == "" || strings.Contains(subdirectory, "..") {
		return b, fmt.Errorf("invalid native backup selection")
	}
	// Backup HLC endpoints can contain sub-microsecond wall time. The pgwire
	// binary timestamp representation can round that endpoint forward, outside
	// the native manifest's coverage. Select a supported timestamp inside the
	// coverage before encoding it, and use that exact cutoff for every read and
	// restore. Never adjust a timestamp from an already published point.
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT backup_type,date_trunc('microsecond',start_time) AS start_time,date_trunc('microsecond',end_time) AS end_time,is_full_cluster FROM [SHOW BACKUP FROM `+literal(subdirectory)+` IN `+literal(collection)+` WITH check_files] ORDER BY end_time`)
	if err != nil {
		return b, fmt.Errorf("native backup verification failed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var start sql.NullTime
		var end time.Time
		var fullCluster bool
		if err := rows.Scan(&kind, &start, &end, &fullCluster); err != nil {
			return b, err
		}
		if !fullCluster {
			return b, fmt.Errorf("recovery requires a full-cluster backup covering every state database")
		}
		layer := Layer{End: end}
		if start.Valid {
			layer.Start = start.Time
		}
		if len(b.Layers) == 0 && kind != "full" {
			return b, fmt.Errorf("backup chain is missing its full layer")
		}
		b.Layers = append(b.Layers, layer)
	}
	return b, rows.Err()
}

// FreezeDatabase checksums exact immutable versions of every object in the
// selected full and incremental directories, including manifests and metadata.
// A new incremental racing this walk cannot make an incomplete chain pass
// InspectBackup at the selected timestamp; publication also rechecks the chain.
func (s Service) FreezeDatabase(ctx context.Context, b Database, objectPrefixes []string) (Database, error) {
	seen := map[string]bool{}
	for _, prefix := range objectPrefixes {
		objects, err := s.Storage.Versions(ctx, prefix)
		if err != nil {
			return b, err
		}
		for _, o := range objects {
			if seen[o.Key] {
				continue
			}
			seen[o.Key] = true // S3 returns newest versions first per key
			f, err := temporary()
			if err != nil {
				return b, err
			}
			f.Close()
			if err = s.Storage.Get(ctx, o, f.Name()); err == nil {
				o.Digest, o.Size, err = FileDigest(f.Name())
			}
			os.Remove(f.Name())
			if err != nil {
				return b, err
			}
			b.Objects = append(b.Objects, o)
		}
	}
	return b, nil
}
