package deploy

import (
	"context"
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
	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/recovery"
)

// RunRecovery is usable without a platform session or the deployment controller.
// pgBackRest timers on the database host, WAL archiving, an OS timer for
// completion, and an external monitor are independently operated processes.
func RunRecovery(ctx context.Context, args []string, out io.Writer) (returnErr error) {
	if len(args) == 0 {
		return fmt.Errorf("usage: platformctl recovery <protect|protect-files|verify-files|schedule|finalize|complete|verify|monitor|collect|drill> [flags]")
	}
	if contains([]string{"empty-destination", "fleet-inventory", "reserve-fleet", "reset-authority", "initialize-authority", "resume-authority"}, args[0]) {
		return runFleetRecovery(ctx, args, out)
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
	fs.StringVar(&subdir, "backup-label", "", "pgBackRest backup label; empty selects the newest backup covering the snapshot")
	fs.StringVar(&timestamp, "timestamp", "", "must equal the live snapshot timestamp when set; PostgreSQL inventory cannot be read in the past")
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
	if contains([]string{"protect-files", "verify-files", "schedule", "finalize", "complete"}, args[0]) {
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
	db, err := platformdb.OpenURL(ctx, strings.TrimSpace(string(url)), platformdb.SchemaPlatform)
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "schedule":
		// Backup cadence is systemd timers on the database host, installed by
		// production operations; nothing is scheduled inside PostgreSQL.
		// This command registers the release inventory and verifies the
		// archiving + repository path those timers feed.
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
		if err := recovery.VerifyArchiving(ctx, db, c.PgBackRest.Stanza); err != nil {
			return err
		}
		if _, err := c.Backups().Info(ctx); err != nil {
			return fmt.Errorf("backup repository is not readable for stanza %q: %w", c.PgBackRest.Stanza, err)
		}
		return nil
	case "finalize":
		return recovery.FinishRelease(ctx, db, c.Installation, c.Release)
	case "complete":
		if err := recovery.VerifyArchiving(ctx, db, c.PgBackRest.Stanza); err != nil {
			return err
		}
		snapshot, walSegment, err := recovery.ReadSnapshot(ctx, db, c.Installation, c.ConsoleSchema)
		if err != nil {
			return err
		}
		t := snapshot.Timestamp
		if timestamp != "" {
			selected, err := time.Parse(time.RFC3339Nano, timestamp)
			if err != nil {
				return err
			}
			if !selected.Equal(t) {
				return fmt.Errorf("--timestamp %s predates the live snapshot %s; PostgreSQL inventory is read live and bound to WAL replay, so complete a fresh point instead", selected, t)
			}
		}
		failureReport.Timestamp = t
		if err := recovery.EnsureWALArchived(ctx, db, walSegment); err != nil {
			return err
		}
		chain, err := recovery.InspectRepository(ctx, c.Backups(), c.PgBackRest.Stanza, c.Repository(), subdir, t, walSegment)
		if err != nil {
			return err
		}
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
				images, releaseAccess, err := recovery.ImageAccess(ctx, db, c.KeyringFile, c.Images, r.ID, false)
				if err != nil {
					return err
				}
				defer releaseAccess()
				if _, err := s.ProtectImage(ctx, images, r.ID); err != nil {
					return err
				}
			default:
				return fmt.Errorf("missing protected %s/%s; protect release files before deployment", r.Kind, r.ID)
			}
		}
		if strings.Trim(c.BackupPrefix, "/") == "" {
			return fmt.Errorf("backup repository prefix is required")
		}
		chain, err = s.FreezeDatabase(ctx, chain, c.BackupPrefix)
		if err != nil {
			return err
		}
		// A backup completing during the version walk can extend the chain.
		// Retry on the next timer tick instead of publishing a mixed view.
		checked, err := recovery.InspectRepository(ctx, c.Backups(), c.PgBackRest.Stanza, c.Repository(), subdir, t, walSegment)
		if err != nil {
			return err
		}
		if checked.BackupLabel != chain.BackupLabel || Digest(checked.Chain) != Digest(chain.Chain) || checked.WALStart != chain.WALStart {
			return fmt.Errorf("backup chain changed during capture; retry completion")
		}
		p := recovery.Point{Installer: c.Requirements(), Version: 1, Installation: c.Installation, Snapshot: snapshot, Database: chain}
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
