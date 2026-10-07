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

type Layer struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type Database struct {
	Collection   string   `json:"collection"` // credential-free external connection URI
	Subdirectory string   `json:"subdirectory"`
	Layers       []Layer  `json:"layers"`
	Objects      []Object `json:"objects"`
}

type Point struct {
	Version      int          `json:"version"`
	Installation string       `json:"installation"`
	Snapshot     Snapshot     `json:"snapshot"`
	Database     Database     `json:"database"`
	Dependencies []Dependency `json:"dependencies"`
	CompletedAt  time.Time    `json:"completedAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
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
	layers := p.Database.Layers
	if p.Database.Collection == "" || p.Database.Subdirectory == "" || len(p.Database.Objects) == 0 || len(layers) == 0 || len(layers) > 49 || !layers[0].Start.IsZero() || t.Before(layers[0].End) || t.After(layers[len(layers)-1].End) {
		return fmt.Errorf("database chain must contain a full backup and at most 48 incrementals covering the selected timestamp")
	}
	for n := 1; n < len(layers); n++ {
		if !layers[n].Start.Equal(layers[n-1].End) || !layers[n].End.After(layers[n].Start) {
			return fmt.Errorf("database chain has a gap at layer %d", n)
		}
	}
	seen := map[string]bool{}
	kinds := map[string]bool{}
	for _, r := range p.Snapshot.Requirements {
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
	for _, r := range p.Snapshot.Requirements {
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
