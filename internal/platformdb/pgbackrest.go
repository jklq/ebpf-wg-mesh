package platformdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Backup tooling is the second half of the provider seam: recovery logic
// depends on Backups, and pgBackRest (with PostgreSQL WAL archiving) is the
// adapter. A different provider would bring its own adapter; recovery-point
// manifests record the provider-neutral BackupInfo below.

// BackupType is a pgBackRest backup kind.
type BackupType string

const (
	BackupFull        BackupType = "full"
	BackupIncremental BackupType = "incr"
	BackupDiff        BackupType = "diff"
)

// BackupInfo is one completed backup in a stanza repository.
type BackupInfo struct {
	Label     string
	Type      BackupType
	Prior     string // label this backup depends on, empty for full
	Timeline  int
	WALStart  string
	WALStop   string
	StartTime time.Time
	StopTime  time.Time
	Error     bool
	LSNStart  string
	LSNStop   string
}

// ArchiveRange is the archived WAL coverage of one timeline.
type ArchiveRange struct {
	Timeline int
	Min      string
	Max      string
}

// RepoInfo is the inspected state of one stanza repository.
type RepoInfo struct {
	Name     string
	Backups  []BackupInfo
	Archives []ArchiveRange
}

// Backups is the interface recovery logic uses for physical backup,
// inspection, and point-in-time restore.
type Backups interface {
	// CreateStanza binds the primary (fresh, or restored onto a new
	// timeline) to its repository stanza. It is idempotent.
	CreateStanza(ctx context.Context) error
	// Check verifies the stanza, repository, and WAL archiving end to end.
	Check(ctx context.Context) error
	// Backup runs one backup of the given type and returns after pgBackRest
	// has recorded it in the repository.
	Backup(ctx context.Context, kind BackupType) error
	Info(ctx context.Context) (RepoInfo, error)
	// Restore materializes label into dataDir (which it replaces) and arms
	// recovery to targetTime. The caller starts the server afterwards; an
	// empty targetTime recovers to the end of archived WAL.
	Restore(ctx context.Context, dataDir, label string, targetTime time.Time) error
	// Expire applies repository retention. Callers run it only after a new
	// complete recovery point has frozen the objects it still needs.
	Expire(ctx context.Context) error
}

// CommandRunner runs one external command. It is the internal seam tests use
// to exercise pgBackRest argument and output handling without a server.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// PgBackRest is the pgBackRest adapter for Backups.
type PgBackRest struct {
	Binary     string // pgbackrest executable
	ConfigFile string // --config; empty uses pgBackRest defaults
	Stanza     string
	Run        CommandRunner // nil uses exec
}

func (p PgBackRest) run(ctx context.Context, args ...string) ([]byte, error) {
	if p.Binary == "" || p.Stanza == "" {
		return nil, fmt.Errorf("pgBackRest binary and stanza are required")
	}
	full := []string{"--stanza=" + p.Stanza}
	if p.ConfigFile != "" {
		full = append(full, "--config="+p.ConfigFile)
	}
	full = append(full, args...)
	runner := p.Run
	if runner == nil {
		runner = execRunner
	}
	return runner(ctx, p.Binary, full...)
}

func (p PgBackRest) CreateStanza(ctx context.Context) error {
	_, err := p.run(ctx, "stanza-create")
	return err
}

func (p PgBackRest) Check(ctx context.Context) error {
	_, err := p.run(ctx, "check")
	return err
}

func (p PgBackRest) Backup(ctx context.Context, kind BackupType) error {
	if kind != BackupFull && kind != BackupIncremental && kind != BackupDiff {
		return fmt.Errorf("invalid backup type %q", kind)
	}
	// --start-fast makes the first checkpoint immediate so scheduled
	// incremental backups keep the recovery objective instead of waiting
	// for the next spread checkpoint.
	_, err := p.run(ctx, "backup", "--type="+string(kind), "--start-fast")
	return err
}

func (p PgBackRest) Expire(ctx context.Context) error {
	_, err := p.run(ctx, "expire")
	return err
}

func (p PgBackRest) Restore(ctx context.Context, dataDir, label string, targetTime time.Time) error {
	if dataDir == "" || label == "" {
		return fmt.Errorf("pgBackRest restore requires a data directory and backup label")
	}
	args := []string{"restore", "--pg1-path=" + dataDir, "--set=" + label, "--type=time", "--target-action=promote"}
	if !targetTime.IsZero() {
		args = append(args, "--target="+targetTime.UTC().Format("2006-01-02 15:04:05.999999-07"))
	}
	_, err := p.run(ctx, args...)
	return err
}

type pgbrInfo []struct {
	Name   string `json:"name"`
	Backup []struct {
		Label   string `json:"label"`
		Prior   string `json:"prior"`
		Type    string `json:"type"`
		Error   bool   `json:"error"`
		Archive struct {
			Start string `json:"start"`
			Stop  string `json:"stop"`
		} `json:"archive"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		LSN struct {
			Start string `json:"start"`
			Stop  string `json:"stop"`
		} `json:"lsn"`
	} `json:"backup"`
	Archive []struct {
		Database struct {
			ID int `json:"id"`
		} `json:"db"`
		ID  string `json:"id"`
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"archive"`
}

// ParseInfo parses `pgbackrest info --output=json` output. The timeline is
// the first component of the archive id ("<timeline>-<db history id>" in
// pgBackRest's "<version>-<id>" archive naming it is the WAL timeline
// encoded in segment names); for backups it comes from the stop segment.
func ParseInfo(raw []byte, stanza string) (RepoInfo, error) {
	var doc pgbrInfo
	if err := json.Unmarshal(raw, &doc); err != nil {
		return RepoInfo{}, fmt.Errorf("parse pgBackRest info: %w", err)
	}
	for _, s := range doc {
		if s.Name != stanza {
			continue
		}
		info := RepoInfo{Name: s.Name}
		for _, b := range s.Backup {
			timeline := 0
			if tl, _, err := ParseWALSegment(b.Archive.Stop); err == nil {
				timeline = tl
			}
			info.Backups = append(info.Backups, BackupInfo{
				Label: b.Label, Type: BackupType(b.Type), Prior: b.Prior,
				Timeline: timeline, WALStart: b.Archive.Start, WALStop: b.Archive.Stop,
				StartTime: time.Unix(b.Timestamp.Start, 0).UTC(),
				StopTime:  time.Unix(b.Timestamp.Stop, 0).UTC(),
				Error:     b.Error, LSNStart: b.LSN.Start, LSNStop: b.LSN.Stop,
			})
		}
		for _, a := range s.Archive {
			timeline := 0
			if tl, _, err := ParseWALSegment(a.Max); err == nil {
				timeline = tl
			}
			info.Archives = append(info.Archives, ArchiveRange{Timeline: timeline, Min: a.Min, Max: a.Max})
		}
		return info, nil
	}
	return RepoInfo{}, fmt.Errorf("pgBackRest stanza %q not found in info output", stanza)
}

func (p PgBackRest) Info(ctx context.Context) (RepoInfo, error) {
	raw, err := p.run(ctx, "info", "--output=json")
	if err != nil {
		return RepoInfo{}, err
	}
	return ParseInfo(raw, p.Stanza)
}

// SelectBackup picks the newest error-free backup whose stop time is at or
// before t, preferring the latest chain link so PITR replays the least WAL.
// It returns the full dependency chain root label via Prior links resolved
// by the caller against RepoInfo.
func SelectBackup(info RepoInfo, t time.Time) (BackupInfo, error) {
	var best BackupInfo
	found := false
	for _, b := range info.Backups {
		if b.Error || b.StopTime.After(t) {
			continue
		}
		if !found || b.StopTime.After(best.StopTime) {
			best, found = b, true
		}
	}
	if !found {
		return BackupInfo{}, fmt.Errorf("no completed backup covers %s", t.UTC().Format(time.RFC3339Nano))
	}
	return best, nil
}

// ChainLabels resolves label's Prior links into the full set of repository
// backup labels a restore of label needs, root (full) first.
func ChainLabels(info RepoInfo, label string) ([]string, error) {
	byLabel := map[string]BackupInfo{}
	for _, b := range info.Backups {
		byLabel[b.Label] = b
	}
	var chain []string
	seen := map[string]bool{}
	for cur := label; cur != ""; {
		if seen[cur] {
			return nil, fmt.Errorf("backup dependency cycle at %s", cur)
		}
		seen[cur] = true
		b, ok := byLabel[cur]
		if !ok {
			return nil, fmt.Errorf("backup %s missing from repository info", cur)
		}
		chain = append([]string{cur}, chain...)
		cur = b.Prior
	}
	if len(chain) == 0 || byLabel[chain[0]].Type != BackupFull {
		return nil, fmt.Errorf("backup chain for %s has no full backup root", label)
	}
	return chain, nil
}

// ArchiveCovers reports whether archived WAL on timeline reaches segment.
func ArchiveCovers(info RepoInfo, timeline int, segment string) bool {
	for _, a := range info.Archives {
		if a.Timeline != timeline || a.Max == "" {
			continue
		}
		if cmp, err := CompareWALSegments(a.Max, segment); err == nil && cmp >= 0 {
			return true
		}
	}
	return false
}
