package recovery

import (
	"fmt"
	"net/url"
	"strings"

	ini "gopkg.in/ini.v1"
)

// RenderPgBackRestConfig renders the one pgBackRest configuration every
// participant shares, derived from the independently verified recovery
// storage instead of a second hand-maintained service selection: the S3
// repository is the recovery bucket under backupPrefix, with the writer
// credentials from the storage profile. pgPath is the PostgreSQL data
// directory on the database host; completers pass empty and get a
// repository-only configuration for `pgbackrest info`. The output contains
// the S3 secret key and must stay in private files.
func RenderPgBackRestConfig(storage StorageConfig, backupPrefix, stanza, pgPath string) (string, error) {
	if err := storage.Validate(); err != nil {
		return "", err
	}
	if stanza == "" || strings.ContainsAny(stanza, " \n\r[]") {
		return "", fmt.Errorf("pgBackRest stanza is required")
	}
	b, err := privateFile(storage.CredentialsFile)
	if err != nil {
		return "", err
	}
	file, err := ini.Load(b)
	if err != nil {
		return "", fmt.Errorf("invalid S3 credentials")
	}
	section, err := file.GetSection(storage.Profile)
	if err != nil {
		return "", fmt.Errorf("S3 credential profile is absent")
	}
	access, secret := section.Key("aws_access_key_id").String(), section.Key("aws_secret_access_key").String()
	if access == "" || secret == "" {
		return "", fmt.Errorf("S3 profile requires explicit access credentials")
	}
	endpoint := ""
	if storage.Endpoint != "" {
		u, err := url.Parse(storage.Endpoint)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("invalid recovery S3 endpoint")
		}
		endpoint = u.Host
	}
	var out strings.Builder
	out.WriteString("[global]\n")
	out.WriteString("repo1-type=s3\n")
	out.WriteString("repo1-path=/" + strings.Trim(backupPrefix, "/") + "\n")
	out.WriteString("repo1-s3-bucket=" + storage.Bucket + "\n")
	out.WriteString("repo1-s3-region=" + storage.Region + "\n")
	if endpoint != "" {
		out.WriteString("repo1-s3-endpoint=" + endpoint + "\n")
	}
	out.WriteString("repo1-s3-key=" + access + "\n")
	out.WriteString("repo1-s3-key-secret=" + secret + "\n")
	if token := section.Key("aws_session_token").String(); token != "" {
		out.WriteString("repo1-s3-token=" + token + "\n")
	}
	if storage.CAFile != "" {
		out.WriteString("repo1-storage-ca-file=" + storage.CAFile + "\n")
	}
	// Retention mirrors the recovery-point lifetime: keep at least the full
	// backups a 30-day point can still reference. Expiry runs only after a
	// newer point has frozen its objects into the protected manifest.
	out.WriteString("repo1-retention-full=8\n")
	out.WriteString("repo1-retention-diff=4\n")
	out.WriteString("repo1-retention-archive-type=full\n")
	out.WriteString("archive-async=y\n")
	out.WriteString("archive-push-queue-max=1GiB\n")
	out.WriteString("process-max=2\n")
	out.WriteString("log-level-console=info\n")
	out.WriteString("start-fast=y\n")
	if pgPath != "" {
		// Keep every pgBackRest working path beside the data directory it
		// serves, under the database service account.
		base := pgPath[:len(pgPath)-len("/data")]
		if !strings.HasSuffix(pgPath, "/data") {
			base = pgPath
		}
		out.WriteString("spool-path=" + base + "/pgbackrest-spool\n")
		out.WriteString("lock-path=" + base + "/pgbackrest-lock\n")
		out.WriteString("log-path=" + base + "/pgbackrest-log\n")
	}
	out.WriteString("\n[" + stanza + "]\n")
	if pgPath != "" {
		base := pgPath[:len(pgPath)-len("/data")]
		if !strings.HasSuffix(pgPath, "/data") {
			base = pgPath
		}
		out.WriteString("pg1-path=" + pgPath + "\n")
		out.WriteString("pg1-user=root\n")
		out.WriteString("pg1-port=5432\n")
		// Matches the release's unix_socket_directories setting.
		out.WriteString("pg1-socket-path=" + base + "\n")
	}
	return out.String(), nil
}

// RenderPgBackRestPosixConfig renders the restore-only configuration that
// reads a materialized copy of the frozen repository objects. Restores
// never read the live S3 repository: the exact versions recorded in the
// recovery point are uploaded to the destination host (or materialized in
// the drill workspace) and served from disk, so repository mutation after
// verification cannot change what gets restored.
func RenderPgBackRestPosixConfig(stanza, pgPath, repoPath string) string {
	var out strings.Builder
	out.WriteString("[global]\n")
	out.WriteString("repo1-type=posix\n")
	out.WriteString("repo1-path=" + repoPath + "\n")
	out.WriteString("process-max=2\n")
	out.WriteString("log-level-console=info\n")
	out.WriteString("\n[" + stanza + "]\n")
	out.WriteString("pg1-path=" + pgPath + "\n")
	out.WriteString("pg1-user=root\n")
	return out.String()
}

// BackupTimers are the systemd cadences that replace the in-database
// CockroachDB backup schedules. They live with the recovery configuration
// so installation and verification render identical units.
const (
	FullBackupSchedule        = "*-*-* 00/6:00:00"
	IncrementalBackupSchedule = "*-*-* *:00/10:00"
)
