package recovery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type File struct {
	Requirement Requirement `json:"requirement"`
	Path        string      `json:"path"`
	Secret      bool        `json:"secret"`
}

type Config struct {
	Storage          StorageConfig `json:"storage"`
	RecoveryKeyFile  string        `json:"recoveryKeyFile"`
	Installation     string        `json:"installation"`
	Release          string        `json:"release"`
	DatabaseURLFile  string        `json:"databaseURLFile"`
	ConsoleSchema    string        `json:"consoleSchema"`
	BackupConnection string        `json:"backupConnection"`
	BackupPrefix     string        `json:"backupPrefix"`
	KeyringFile      string        `json:"keyringFile"`
	Images           Images        `json:"images"`
	Files            []File        `json:"files"`
	// DrillCommand is a pinned release tool running inside a fresh network/PID
	// namespace. It provisions the isolated DB and registry from materialized data.
	DrillCommand        []string `json:"drillCommand"`
	DrillCommandDigest  string   `json:"drillCommandDigest"`
	CapacityAssumptions string   `json:"capacityAssumptions"`
	FleetSize           int      `json:"fleetSize"`
}

func Load(path string) (Config, Service, error) {
	var c Config
	b, err := privateFile(path)
	if err != nil {
		return c, Service{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, Service{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, Service{}, fmt.Errorf("recovery configuration must contain exactly one JSON document")
	}
	if err := c.Storage.Validate(); err != nil {
		return c, Service{}, err
	}
	key, err := privateFile(c.RecoveryKeyFile)
	if err != nil {
		return c, Service{}, err
	}
	if len(key) != 32 {
		return c, Service{}, fmt.Errorf("recovery encryption key must contain 32 raw bytes")
	}
	keyring, err := os.ReadFile(c.KeyringFile)
	if err == nil {
		var ring struct {
			Keys map[string]struct {
				Key string `json:"key"`
			}
		}
		if json.Unmarshal(keyring, &ring) == nil {
			for _, entry := range ring.Keys {
				material, _ := base64.StdEncoding.DecodeString(entry.Key)
				if bytes.Equal(material, key) {
					return c, Service{}, fmt.Errorf("recovery key must be independent of the platform keyring")
				}
			}
		}
	}
	return c, Service{Storage: &S3{Config: c.Storage}, Prefix: c.Storage.Prefix, RecoveryKey: key}, nil
}

func (c Config) Requirements() []Requirement {
	var out []Requirement
	for _, f := range c.Files {
		out = append(out, f.Requirement)
	}
	return out
}

func (s Service) ProtectFiles(ctx context.Context, c Config) error {
	if err := s.Storage.Check(ctx); err != nil {
		return err
	}
	for _, f := range c.Files {
		if f.Requirement.Kind == "keyring" || f.Requirement.Kind == "console-key" || f.Requirement.Kind == "external-secret" {
			if !f.Secret {
				return fmt.Errorf("%s must be encrypted before upload", identity(f.Requirement))
			}
		}
		if _, err := s.Protect(ctx, f.Requirement, f.Path, f.Secret, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s Service) RequireFiles(ctx context.Context, c Config) error {
	for _, f := range c.Files {
		if f.Secret {
			if err := s.RequireSecret(ctx, f.Requirement, f.Path); err != nil {
				return err
			}
			continue
		}
		d, err := s.Find(ctx, f.Requirement, time.Now().Add(Retention))
		if err != nil {
			return err
		}
		if err := s.verifyObject(ctx, d.Objects[0], time.Now().Add(Retention), true); err != nil {
			return err
		}
		local, _, err := FileDigest(f.Path)
		if err != nil {
			return err
		}
		if local != d.Objects[0].Digest {
			return fmt.Errorf("local %s differs from protected release material", identity(f.Requirement))
		}
	}
	return nil
}

// ProtectArchive is shared by timestamped capture and the active deletion gate.
// read streams from the active backend; a failed copy prevents deletion.
func (s Service) ProtectArchive(ctx context.Context, key, digest string, read func(io.Writer) error) error {
	r := Requirement{Kind: "source", ID: key, Digest: digest}
	if _, err := s.Find(ctx, r, time.Now().Add(Retention)); err == nil {
		return nil
	}
	f, err := temporary()
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := read(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = s.Protect(ctx, r, f.Name(), false, nil)
	return err
}
