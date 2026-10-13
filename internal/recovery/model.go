// Package recovery owns independent, version-pinned recovery points. A database
// backup is only one dependency; publication follows verification of the whole graph.
package recovery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ebof-wg-mesh/internal/platformdb"
)

const (
	Retention       = 30 * 24 * time.Hour
	Objective       = 15 * time.Minute
	FullCron        = "0 */6 * * *"
	IncrementalCron = "*/10 * * * *"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Digest(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

type Object struct {
	Key         string    `json:"key"`
	Version     string    `json:"version"`
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	RetainUntil time.Time `json:"retainUntil"`
}

func (o Object) S3URL(bucket string) string {
	return (&url.URL{Scheme: "s3", Host: bucket, Path: "/" + o.Key, RawQuery: url.Values{"versionId": {o.Version}}.Encode()}).String()
}

// Dependency identity describes the original artifact, independently of its
// encrypted bundle or export's storage digest.
type Dependency struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	Digest    string   `json:"digest,omitempty"`
	Objects   []Object `json:"objects"`
	Inventory []string `json:"inventory,omitempty"` // complete OCI descriptor closure
}

type Requirement struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Digest string `json:"digest,omitempty"`
}

type Snapshot struct {
	WrapProbes     []WrapProbe         `json:"wrapProbes,omitempty"`
	ConsoleProbes  []ConsoleProbe      `json:"consoleProbes,omitempty"`
	Timestamp      time.Time           `json:"timestamp"`
	Requirements   []Requirement       `json:"requirements"`
	ExternalImages []string            `json:"externalImages,omitempty"`
	Identities     map[string][]string `json:"identities"`
	Schema         int                 `json:"schema"`
	ConsoleSchema  int                 `json:"consoleSchema"`
}

// Database identifies the physical backup chain and archived WAL range a
// point-in-time restore needs. It is written by the backup adapter
// (pgBackRest today) and recorded verbatim in the protected manifest, so a
// restore never has to trust mutable repository state: the exact backup
// label, its dependency chain, and the WAL segments covering the snapshot
// timestamp are pinned here and frozen as Objects.
type Database struct {
	Provider string `json:"provider"` // platformdb provider name, e.g. "postgres"
	Stanza   string `json:"stanza"`
	// BackupLabel is the selected backup; Chain lists every label a restore
	// of it needs, full backup first, BackupLabel last.
	BackupLabel string    `json:"backupLabel"`
	Chain       []string  `json:"chain"`
	Timeline    int       `json:"timeline"`
	BackupStop  time.Time `json:"backupStop"` // completion time of BackupLabel
	WALStart    string    `json:"walStart"`   // first WAL segment the chain needs
	WALStop     string    `json:"walStop"`    // last archived WAL segment frozen for this point
	Repository  string    `json:"repository"` // credential-free repo identity, e.g. s3://bucket/prefix
	Objects     []Object  `json:"objects"`
}

type Point struct {
	// Installer selects the current independently observed host inventory and
	// operator inputs. They may postdate the SQL cutoff; retaining them prevents
	// reuse of identities/ranges introduced after that cutoff.
	Installer    []Requirement `json:"installer"`
	Version      int           `json:"version"`
	Installation string        `json:"installation"`
	Snapshot     Snapshot      `json:"snapshot"`
	Database     Database      `json:"database"`
	Dependencies []Dependency  `json:"dependencies"`
	CompletedAt  time.Time     `json:"completedAt"`
	ExpiresAt    time.Time     `json:"expiresAt"`
}

type Report struct {
	Complete   bool      `json:"complete"`
	Timestamp  time.Time `json:"timestamp"`
	AgeSeconds float64   `json:"ageSeconds,omitempty"`
	Missing    []string  `json:"missing,omitempty"`
	Failures   []string  `json:"failures,omitempty"`
}

// Validate catches omission, chain gaps, mutable identities, and timestamp
// mistakes before any point can become a complete catalog entry.
func (p Point) Validate(now time.Time) error {
	t := p.Snapshot.Timestamp
	if p.Version != 1 || p.Installation == "" || t.IsZero() || t.After(now) || !p.ExpiresAt.Equal(t.Add(Retention)) || !now.Before(p.ExpiresAt) {
		return fmt.Errorf("invalid recovery point identity, timestamp, or retention")
	}
	if p.CompletedAt.IsZero() || p.CompletedAt.Before(t) || p.CompletedAt.After(now) {
		return fmt.Errorf("invalid recovery completion time")
	}
	if p.Snapshot.Schema <= 0 || p.Snapshot.ConsoleSchema <= 0 || len(p.Snapshot.Identities) == 0 {
		return fmt.Errorf("recovery point lacks schemas or platform identities")
	}
	for _, table := range []string{"agent_registrations", "ingress_nodes", "platform_signing_keys"} {
		if _, ok := p.Snapshot.Identities[table]; !ok {
			return fmt.Errorf("recovery point lacks %s identities", table)
		}
	}
	db := p.Database
	if db.Provider == "" || db.Stanza == "" || db.BackupLabel == "" || db.Repository == "" || len(db.Objects) == 0 || len(db.Chain) == 0 || len(db.Chain) > 64 || db.Chain[len(db.Chain)-1] != db.BackupLabel || db.BackupStop.IsZero() || t.Before(db.BackupStop) {
		return fmt.Errorf("database backup chain must end at the selected backup and cover the selected timestamp")
	}
	seenLabels := map[string]bool{}
	for _, label := range db.Chain {
		if label == "" || seenLabels[label] {
			return fmt.Errorf("database backup chain has an empty or duplicate label")
		}
		seenLabels[label] = true
	}
	startTimeline, startSeq, startErr := platformdb.ParseWALSegment(db.WALStart)
	stopTimeline, stopSeq, stopErr := platformdb.ParseWALSegment(db.WALStop)
	if startErr != nil || stopErr != nil || startTimeline != db.Timeline || stopTimeline != db.Timeline || stopSeq < startSeq {
		return fmt.Errorf("database WAL range does not cover the selected backup timeline")
	}
	selected := map[string]int{}
	for _, r := range p.Installer {
		selected[r.Kind]++
	}
	for _, kind := range []string{"installation", "release", "deployment-state", "external-secret"} {
		if selected[kind] != 1 {
			return fmt.Errorf("installer requires exactly one selected %s", kind)
		}
	}
	seen := map[string]bool{}
	kinds := map[string]bool{}
	for _, r := range p.Requirements() {
		key := r.Kind + "/" + r.ID
		if r.ID == "" || seen[key] || (r.Digest != "" && !digestPattern.MatchString(r.Digest)) {
			return fmt.Errorf("invalid or duplicate requirement %s", key)
		}
		seen[key], kinds[r.Kind] = true, true
		if r.Kind == "image" && (!digestPattern.MatchString(r.Digest) || !strings.HasSuffix(r.ID, "@"+r.Digest)) {
			return fmt.Errorf("image recovery identities must be pinned by digest")
		}
	}
	for _, kind := range []string{"keyring", "console-key", "external-secret", "installation", "deployment-state", "release", "tool"} {
		if !kinds[kind] {
			return fmt.Errorf("recovery point is missing %s requirements", kind)
		}
	}
	return nil
}

// Requirements combines the historical platform graph with independently
// selected installer inputs. Identical requirements need only one protected copy.
func (p Point) Requirements() []Requirement {
	out := append([]Requirement{}, p.Snapshot.Requirements...)
	seen := map[Requirement]bool{}
	for _, r := range out {
		seen[r] = true
	}
	for _, r := range p.Installer {
		if !seen[r] {
			out = append(out, r)
			seen[r] = true
		}
	}
	return out
}

func (p Point) ManifestDigest() string { return Digest(append(jsonBytes(p), '\n')) }

func identity(r Requirement) string { return r.Kind + "/" + r.ID }

func (p Point) ValidateDependencies() error {
	seen := map[string]Dependency{}
	for _, d := range p.Dependencies {
		if _, ok := seen[d.Kind+"/"+d.ID]; ok {
			return fmt.Errorf("duplicate recovery dependency")
		}
		seen[d.Kind+"/"+d.ID] = d
	}
	check := func(o Object) error {
		if o.Version == "" || o.Version == "null" || o.Key == "" || !digestPattern.MatchString(o.Digest) || o.Size < 0 || o.RetainUntil.Before(p.ExpiresAt) {
			return fmt.Errorf("incomplete protected object inventory or retention")
		}
		return nil
	}
	for _, o := range p.Database.Objects {
		if err := check(o); err != nil {
			return err
		}
	}
	for _, r := range p.Requirements() {
		d, ok := seen[identity(r)]
		if !ok || d.Digest != r.Digest || len(d.Objects) == 0 || (d.Kind == "image" && len(d.Inventory) == 0) {
			return fmt.Errorf("missing protected dependency %s", identity(r))
		}
		for _, o := range d.Objects {
			if err := check(o); err != nil {
				return err
			}
		}
	}
	return nil
}
func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
