package recovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/platformdb"
)

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// VerifyArchiving checks the live server is feeding the WAL archive the
// recovery points replay from: archiving on at replica level, the archive
// command is the backup adapter, and the newest archived segment is fresh
// enough for the recovery objective. Backup *scheduling* lives outside the
// database (systemd timers on the database host); there is nothing to
// install or inspect over SQL since the CockroachDB schedulers were removed.
func VerifyArchiving(ctx context.Context, db *sql.DB, stanza string) error {
	var mode, level, command string
	if err := db.QueryRowContext(ctx, `SELECT current_setting('archive_mode'), current_setting('wal_level'), current_setting('archive_command')`).Scan(&mode, &level, &command); err != nil {
		return err
	}
	if mode != "on" && mode != "always" {
		return fmt.Errorf("WAL archiving is not enabled (archive_mode=%s)", mode)
	}
	if level == "minimal" {
		return fmt.Errorf("wal_level=%s cannot archive WAL for point-in-time recovery", level)
	}
	if !strings.Contains(command, "pgbackrest") || !strings.Contains(command, "archive-push") || (stanza != "" && !strings.Contains(command, stanza)) {
		return fmt.Errorf("archive_command does not archive WAL with pgBackRest for stanza %q", stanza)
	}
	var lastArchived sql.NullTime
	var failed int64
	if err := db.QueryRowContext(ctx, `SELECT last_archived_time, failed_count FROM pg_stat_archiver`).Scan(&lastArchived, &failed); err != nil {
		return err
	}
	if !lastArchived.Valid {
		return fmt.Errorf("no WAL segment has been archived yet")
	}
	if age := time.Since(lastArchived.Time); age > Objective {
		return fmt.Errorf("newest archived WAL is %s old, beyond the recovery objective", age.Round(time.Second))
	}
	return nil
}

// EnsureWALArchived switches WAL and waits until the archiver has archived
// the segment holding the snapshot, so a published point can always replay
// to its timestamp. It is bounded by ctx.
func EnsureWALArchived(ctx context.Context, db *sql.DB, segment string) error {
	if _, err := db.ExecContext(ctx, `SELECT pg_switch_wal()`); err != nil {
		return fmt.Errorf("switch WAL for snapshot: %w", err)
	}
	for {
		var last string
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_archived_wal,'') FROM pg_stat_archiver`).Scan(&last); err != nil {
			return err
		}
		if last != "" {
			if cmp, err := platformdb.CompareWALSegments(last, segment); err != nil {
				return err
			} else if cmp >= 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("WAL segment %s was not archived: %w", segment, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Register records installer-owned dependencies in the recovery schema so
// inventory reads share the snapshot's database. Keep both releases
// registered during cutover; FinishRelease removes the previous release only
// after platform availability has been independently verified.
func Register(ctx context.Context, db *sql.DB, installation, release string, requirements []Requirement) error {
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS recovery`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS recovery.installations (
	 installation TEXT NOT NULL, release TEXT NOT NULL, requirements JSONB NOT NULL,
	 PRIMARY KEY (installation, release))`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO recovery.installations VALUES ($1,$2,$3)
	 ON CONFLICT (installation, release) DO UPDATE SET requirements=excluded.requirements`,
		installation, release, string(jsonBytes(requirements)))
	return err
}

func FinishRelease(ctx context.Context, db *sql.DB, installation, release string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM recovery.installations WHERE installation=$1 AND release<>$2`, installation, release)
	return err
}

// ReadSnapshot reads the platform inventory in one repeatable-read
// transaction and returns it with the WAL segment that contains every
// commit the snapshot can see. PostgreSQL has no time-travel reads (that
// was the CockroachDB AS OF SYSTEM TIME path); instead the snapshot binds
// a live consistent read to a point-in-time recovery target: restore the
// selected backup, replay archived WAL to Snapshot.Timestamp, and the
// restored database contains exactly what this read saw.
func ReadSnapshot(ctx context.Context, db *sql.DB, installation, consoleSchema string) (Snapshot, string, error) {
	s := Snapshot{Identities: map[string][]string{}}
	if consoleSchema == "" {
		return s, "", fmt.Errorf("console SQL schema is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return s, "", err
	}
	defer tx.Rollback()
	// First statement fixes the snapshot; its transaction timestamp is the
	// recovery target and its WAL segment bounds archived replay.
	var walSegment string
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp(), pg_walfile_name(pg_current_wal_lsn())`).Scan(&s.Timestamp, &walSegment); err != nil {
		return s, "", err
	}
	s.Timestamp = s.Timestamp.UTC()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(version),0) FROM schema_migrations`).Scan(&s.Schema); err != nil {
		return s, "", err
	}
	identifier := platformdb.QuoteIdentifier(consoleSchema)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(version),0) FROM `+identifier+`.schema_migrations`).Scan(&s.ConsoleSchema); err != nil {
		return s, "", fmt.Errorf("console schema inventory: %w", err)
	}
	query := func(statement string, visit func(*sql.Rows) error, args ...any) error {
		rows, err := tx.QueryContext(ctx, statement, args...)
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
	if err := query(`SELECT requirements FROM recovery.installations WHERE installation=$1`, func(rows *sql.Rows) error {
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
	}, installation); err != nil {
		return s, "", err
	}
	if err := query(`SELECT provider_ref FROM envelope_keys`, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.Requirements = append(s.Requirements, Requirement{Kind: "keyring", ID: id})
		return nil
	}); err != nil {
		return s, "", err
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
		return s, "", err
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
		return s, "", err
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
			return s, "", err
		}
		if s.Identities[table] == nil {
			s.Identities[table] = []string{}
		}
	}
	if err := readSecretProbes(ctx, tx, &s, identifier); err != nil {
		return s, "", err
	}
	var hasUsage bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('usage_producers') IS NOT NULL`).Scan(&hasUsage); err != nil {
		return s, "", err
	}
	if hasUsage {
		if err := query(`SELECT installation_id, producer_id, agent_id, state, accepted_through
		 FROM usage_producers ORDER BY installation_id, producer_id`, func(rows *sql.Rows) error {
			var cursor UsageProducerCursor
			var accepted int64
			if err := rows.Scan(&cursor.InstallationID, &cursor.ProducerID, &cursor.AgentID, &cursor.State, &accepted); err != nil {
				return err
			}
			cursor.AcceptedThrough = uint64(accepted)
			s.UsageProducers = append(s.UsageProducers, cursor)
			return nil
		}); err != nil {
			return s, "", fmt.Errorf("usage producer inventory: %w", err)
		}
	}
	unique := map[string]Requirement{}
	for _, r := range s.Requirements {
		k := identity(r)
		if prev, ok := unique[k]; ok && prev.Digest != r.Digest {
			return s, "", fmt.Errorf("historical dependency %s has conflicting immutable identities", k)
		}
		unique[k] = r
	}
	s.Requirements = nil
	for _, r := range unique {
		s.Requirements = append(s.Requirements, r)
	}
	sort.Slice(s.Requirements, func(i, j int) bool { return identity(s.Requirements[i]) < identity(s.Requirements[j]) })
	if err := tx.Commit(); err != nil {
		return s, "", err
	}
	return s, walSegment, nil
}

// InspectRepository selects, through the backup adapter, the newest backup
// chain that can replay archived WAL to the snapshot segment, and returns
// the manifest record for it (without frozen objects). Repository identity
// is credential-free: bucket and prefix only.
// An empty label selects the newest backup that stopped at or before at.
func InspectRepository(ctx context.Context, backups platformdb.Backups, stanza, repository, label string, at time.Time, walSegment string) (Database, error) {
	b := Database{Provider: platformdb.ProviderPostgres, Stanza: stanza, Repository: repository}
	info, err := backups.Info(ctx)
	if err != nil {
		return b, fmt.Errorf("inspect backup repository: %w", err)
	}
	var selected platformdb.BackupInfo
	if label == "" {
		var err error
		if selected, err = platformdb.SelectBackup(info, at); err != nil {
			return b, err
		}
	} else {
		found := false
		for _, candidate := range info.Backups {
			if candidate.Label == label && !candidate.Error && !candidate.StopTime.After(at) {
				selected, found = candidate, true
			}
		}
		if !found {
			return b, fmt.Errorf("backup %s is missing, failed, or stopped after the snapshot", label)
		}
	}
	chain, err := platformdb.ChainLabels(info, selected.Label)
	if err != nil {
		return b, err
	}
	if !platformdb.ArchiveCovers(info, selected.Timeline, walSegment) {
		return b, fmt.Errorf("archived WAL does not yet cover snapshot segment %s", walSegment)
	}
	byLabel := map[string]platformdb.BackupInfo{}
	for _, backup := range info.Backups {
		byLabel[backup.Label] = backup
	}
	b.BackupLabel = selected.Label
	b.Chain = chain
	b.Timeline = selected.Timeline
	b.BackupStop = selected.StopTime
	b.WALStart = byLabel[chain[0]].WALStart
	b.WALStop = walSegment
	return b, nil
}

// RepositoryObjectPrefixes are the repository key prefixes (relative to the
// recovery storage prefix) whose exact object versions FreezeDatabase pins:
// the stanza backup directory and its WAL archive.
func RepositoryObjectPrefixes(b Database, repoPrefix string) []string {
	base := strings.Trim(repoPrefix, "/")
	return []string{
		base + "/backup/" + b.Stanza + "/",
		base + "/archive/" + b.Stanza + "/",
	}
}

// repositoryObjectSelected decides whether one repository object belongs to
// the selected chain or to the frozen WAL range. pgBackRest layout:
// backup/<stanza>/<label>/... plus backup.info metadata, and
// archive/<stanza>/<id>/<segment-prefix>/<segment>-<hash>[.gz].
func repositoryObjectSelected(b Database, key string) bool {
	if strings.Contains(key, "/backup/"+b.Stanza+"/") {
		if strings.Contains(key, "backup.info") {
			return true
		}
		for _, label := range b.Chain {
			if strings.Contains(key, "/"+label+"/") || strings.HasSuffix(key, "/"+label) {
				return true
			}
		}
		return false
	}
	if strings.Contains(key, "/archive/"+b.Stanza+"/") {
		name := key[strings.LastIndexByte(key, '/')+1:]
		// Timeline history files ride along: replay needs them after a
		// promotion, and they are tiny.
		if strings.HasSuffix(name, ".history") {
			return true
		}
		segment := name
		if len(segment) > 24 {
			segment = segment[:24]
		}
		low, errLow := platformdb.CompareWALSegments(segment, b.WALStart)
		high, errHigh := platformdb.CompareWALSegments(segment, b.WALStop)
		return errLow == nil && errHigh == nil && low >= 0 && high <= 0
	}
	return false
}

// FreezeDatabase checksums exact immutable versions of every repository
// object the selected chain and WAL range need, including backup metadata.
// A backup or WAL segment landing during the walk cannot make an incomplete
// chain pass: publication re-inspects the repository and compares the chain.
func (s Service) FreezeDatabase(ctx context.Context, b Database, repoPrefix string) (Database, error) {
	seen := map[string]bool{}
	for _, prefix := range RepositoryObjectPrefixes(b, repoPrefix) {
		objects, err := s.Storage.Versions(ctx, prefix)
		if err != nil {
			return b, err
		}
		for _, o := range objects {
			if seen[o.Key] || !repositoryObjectSelected(b, o.Key) {
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
	if len(b.Objects) == 0 {
		return b, fmt.Errorf("no repository objects found for backup %s under %q", b.BackupLabel, repoPrefix)
	}
	return b, nil
}
