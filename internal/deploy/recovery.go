package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/recovery"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// RunRecovery is usable without a platform session or the deployment controller.
// Native SQL schedules, an OS timer for completion, and an external monitor are
// three independently operated processes.
func RunRecovery(ctx context.Context, args []string, out io.Writer) (returnErr error) {
	if len(args) == 0 {
		return fmt.Errorf("usage: platformctl recovery <protect|protect-files|verify-files|schedule|finalize|complete|verify|monitor|collect|drill> [flags]")
	}
	fs := flag.NewFlagSet("platformctl recovery "+args[0], flag.ContinueOnError)
	var configPath, file, kind, id, digest, pointPath, backupURL, subdir, timestamp, sourceConfig, declared string
	var secret, content bool
	fs.StringVar(&configPath, "config", os.Getenv("PLATFORM_RECOVERY_CONFIG"), "private independent recovery configuration")
	fs.StringVar(&file, "file", "", "artifact or secret file")
	fs.StringVar(&kind, "kind", "", "dependency kind")
	fs.StringVar(&id, "id", "", "immutable dependency identity")
	fs.StringVar(&digest, "digest", "", "original immutable sha256 digest")
	fs.BoolVar(&secret, "secret", false, "encrypt the secret bundle before upload")
	fs.StringVar(&pointPath, "point", "", "protected point Object JSON file")
	fs.StringVar(&backupURL, "backup", "", "version-pinned S3 recovery-point URL")
	fs.StringVar(&subdir, "subdirectory", "", "full backup subdirectory; empty selects latest native full backup")
	fs.StringVar(&timestamp, "timestamp", "", "selected RFC3339 database timestamp; defaults to latest backup endpoint")
	fs.StringVar(&sourceConfig, "source-config", "", "source archive backend JSON file")
	fs.StringVar(&declared, "declared-at", "", "recovery declaration timestamp, before provisioning")
	fs.BoolVar(&content, "check-content", false, "read and hash every protected dependency")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected recovery arguments")
	}
	c, s, err := recovery.Load(configPath)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	if contains([]string{"protect-files", "verify-files", "schedule", "finalize"}, args[0]) {
		for n := range c.Files {
			f := &c.Files[n]
			if !f.Secret && f.Requirement.Digest == "" {
				digest, _, err := recovery.FileDigest(f.Path)
				if err != nil {
					return err
				}
				f.Requirement.Digest = digest
			}
		}
	}

	var failureReport recovery.Report
	if args[0] == "complete" {
		defer func() {
			if returnErr != nil {
				failureReport.Complete = false
				failureReport.Failures = append(failureReport.Failures, returnErr.Error())
				if err := s.RecordFailure(ctx, c.Installation, failureReport); err != nil {
					returnErr = fmt.Errorf("%w; publishing independent failure report also failed: %v", returnErr, err)
				}
			}
		}()
	}

	if err := s.Storage.Check(ctx); err != nil {
		return err
	}
	encode := func(v any) error { return json.NewEncoder(out).Encode(v) }
	switch args[0] {
	case "protect":
		if file == "" || kind == "" || id == "" {
			return fmt.Errorf("protect requires --kind, --id and --file")
		}
		if (kind == "keyring" || kind == "console-key" || kind == "external-secret") && !secret {
			return fmt.Errorf("secret dependencies require --secret")
		}
		d, err := s.Protect(ctx, recovery.Requirement{Kind: kind, ID: id, Digest: digest}, file, secret, nil)
		if err != nil {
			return err
		}
		return encode(d)
	case "protect-files":
		if err := s.ProtectFiles(ctx, c); err != nil {
			return err
		}
		_, err := protectRelease(ctx, s, c, false)
		return err
	case "verify-files":
		if err := s.RequireFiles(ctx, c); err != nil {
			return err
		}
		_, err := protectRelease(ctx, s, c, true)
		return err
	case "monitor":
		r, err := s.CheckFreshness(ctx, c.Installation)
		if e := encode(r); e != nil {
			return e
		}
		return err
	case "collect":
		n, err := s.Collect(ctx)
		if e := encode(map[string]int{"deletedVersions": n}); e != nil {
			return e
		}
		return err
	case "verify", "drill":
		var object recovery.Object
		if (backupURL == "") == (pointPath == "") {
			return fmt.Errorf("select exactly one of --backup or --point")
		}
		if backupURL != "" {
			object, err = s.ResolvePoint(ctx, c.Storage.Bucket, backupURL)
			if err != nil {
				return err
			}
		} else {
			b, err := os.ReadFile(pointPath)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &object); err != nil {
				return err
			}
		}
		p, err := s.ReadPoint(ctx, object)
		if err != nil {
			return err
		}
		r := s.Verify(ctx, p, content || args[0] == "drill")
		if !r.Complete {
			encode(r)
			return fmt.Errorf("recovery point verification failed")
		}
		if err := verifyReleaseInventory(ctx, s, p.Snapshot, p.Dependencies); err != nil {
			return err
		}
		if args[0] == "drill" {
			t, err := time.Parse(time.RFC3339Nano, declared)
			if err != nil {
				return fmt.Errorf("drill requires --declared-at before provisioning")
			}
			result, err := s.Drill(ctx, c, p, t)
			if e := encode(result); e != nil {
				return e
			}
			return err
		}
		return encode(Evidence{Backup: object.S3URL(c.Storage.Bucket), DataLossCutoff: p.Snapshot.Timestamp, Point: &p, Object: &object})
	}
	if c.Installation == "" || c.Release == "" {
		return fmt.Errorf("installation/release identities are required")
	}
	url, err := os.ReadFile(c.DatabaseURLFile)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", strings.TrimSpace(string(url)))
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "schedule":
		if err := recovery.VerifyBackupDestination(ctx, db, c.BackupConnection, c.Storage, c.BackupPrefix); err != nil {
			return err
		}
		if err := s.RequireFiles(ctx, c); err != nil {
			return err
		}
		releaseNeeds, err := protectRelease(ctx, s, c, true)
		if err != nil {
			return err
		}
		if err := recovery.Register(ctx, db, c.Installation, c.Release, append(c.Requirements(), releaseNeeds...)); err != nil {
			return err
		}
		statement, err := recovery.ScheduleSQL(c.Installation, c.BackupConnection)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("native recovery schedule installation failed: %w", err)
		}
		return recovery.VerifySchedules(ctx, db, c.Installation, c.BackupConnection)
	case "finalize":
		return recovery.FinishRelease(ctx, db, c.Installation, c.Release)
	case "complete":
		if err := recovery.VerifyBackupDestination(ctx, db, c.BackupConnection, c.Storage, c.BackupPrefix); err != nil {
			return err
		}
		if err := recovery.VerifySchedules(ctx, db, c.Installation, c.BackupConnection); err != nil {
			return err
		}
		if subdir == "" {
			rows, err := db.QueryContext(ctx, "SHOW BACKUPS IN '"+strings.ReplaceAll(c.BackupConnection, "'", "''")+"'")
			if err != nil {
				return err
			}
			for rows.Next() {
				var path string
				if err := rows.Scan(&path); err != nil {
					rows.Close()
					return err
				}
				if path > subdir {
					subdir = path
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		chain, err := recovery.InspectBackup(ctx, db, c.BackupConnection, subdir)
		if err != nil {
			return err
		}
		if len(chain.Layers) == 0 {
			return fmt.Errorf("no completed native database backup")
		}
		t := chain.Layers[len(chain.Layers)-1].End
		if timestamp != "" {
			t, err = time.Parse(time.RFC3339Nano, timestamp)
			if err != nil {
				return err
			}
		}
		snapshot, err := recovery.ReadSnapshot(ctx, db, c.Installation, c.ConsoleSchema, t)
		if err != nil {
			return err
		}
		failureReport.Timestamp = t
		if err := verifyReleaseInventory(ctx, s, snapshot, nil); err != nil {
			return err
		}
		var archives source.ArchiveStore
		if sourceConfig != "" {
			b, err := os.ReadFile(sourceConfig)
			if err != nil {
				return err
			}
			var cfg config.SourceArchiveConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				return err
			}
			archives, err = source.NewSourceArchiveStore(cfg)
			if err != nil {
				return err
			}
		}
		for _, r := range snapshot.Requirements {
			if _, err := s.Find(ctx, r, t.Add(recovery.Retention)); err == nil {
				continue
			}
			switch r.Kind {
			case "keyring":
				provider, err := secretkeys.NewKeyring(c.KeyringFile, secretkeys.KeyringOptions{})
				if err != nil {
					return err
				}
				present, err := provider.HasKeyMaterial(ctx, r.ID)
				if err != nil || !present {
					return fmt.Errorf("required keyring version %s is unavailable", r.ID)
				}
				if _, err := s.Protect(ctx, r, c.KeyringFile, true, nil); err != nil {
					return err
				}
			case "source":
				if archives == nil {
					return fmt.Errorf("missing protected source %s; --source-config is required for active copies", r.ID)
				}
				if err := s.ProtectArchive(ctx, r.ID, r.Digest, func(w io.Writer) error { return copyArchive(ctx, archives, r.ID, w) }); err != nil {
					return err
				}
			case "image":
				if _, err := s.ProtectImage(ctx, c.Images, r.ID); err != nil {
					return err
				}
			default:
				return fmt.Errorf("missing protected %s/%s; protect release files before deployment", r.Kind, r.ID)
			}
		}
		base := strings.Trim(c.BackupPrefix, "/")
		leaf := strings.Trim(subdir, "/")
		if base == "" || strings.Contains(leaf, "..") {
			return fmt.Errorf("native backup object prefix is required")
		}
		chain, err = s.FreezeDatabase(ctx, chain, []string{base + "/" + leaf + "/", base + "/incrementals/" + leaf + "/"})
		if err != nil {
			return err
		}
		// A backup completing during the version walk can replace collection
		// metadata. Retry on the next timer tick instead of publishing a mixed view.
		checked, err := recovery.InspectBackup(ctx, db, c.BackupConnection, subdir)
		if err != nil {
			return err
		}
		if Digest(chain.Layers) != Digest(checked.Layers) {
			return fmt.Errorf("native backup chain changed during capture; retry completion")
		}
		p := recovery.Point{Version: 1, Installation: c.Installation, Snapshot: snapshot, Database: chain}
		object, report, err := s.Publish(ctx, p)
		failureReport = report
		if err != nil {
			encode(report)
			return err
		}
		p, err = s.ReadPoint(ctx, object)
		if err != nil {
			return err
		}
		return encode(Evidence{Backup: object.S3URL(c.Storage.Bucket), DataLossCutoff: t, Point: &p, Object: &object})
	default:
		return fmt.Errorf("unknown recovery command %s", args[0])
	}
}

func copyArchive(ctx context.Context, store source.ArchiveStore, key string, w io.Writer) error {
	meta, err := store.Stat(ctx, key)
	if err != nil {
		return err
	}
	for offset := int64(0); offset < meta.Size; {
		n := int(min(int64(1<<20), meta.Size-offset))
		b, err := store.ReadRange(ctx, key, offset, n)
		if err != nil {
			return err
		}
		if len(b) != n {
			return io.ErrUnexpectedEOF
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		offset += int64(n)
	}
	return nil
}
