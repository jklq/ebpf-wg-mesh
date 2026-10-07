package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RecoverKeyring merges the individually protected master-key versions. A bundle
// captured before a later rotation need not contain the newer key. Reading only
// the last bundle can omit versions still referenced by restored SQL.
func (s Service) RecoverKeyring(ctx context.Context, p Point, target string) error {
	ring := struct {
		Version int                        `json:"version"`
		Keys    map[string]json.RawMessage `json:"keys"`
	}{Version: 1, Keys: map[string]json.RawMessage{}}
	for _, d := range p.Dependencies {
		if d.Kind != "keyring" {
			continue
		}
		b, err := s.bundle(ctx, d)
		if err != nil {
			return err
		}
		var candidate struct {
			Version int                        `json:"version"`
			Keys    map[string]json.RawMessage `json:"keys"`
		}
		err = json.Unmarshal(b, &candidate)
		clear(b)
		if err != nil || candidate.Version != 1 {
			return fmt.Errorf("invalid protected keyring")
		}
		key, ok := candidate.Keys[d.ID]
		if !ok {
			return fmt.Errorf("protected keyring lacks its named version")
		}
		if previous, ok := ring.Keys[d.ID]; ok && string(previous) != string(key) {
			return fmt.Errorf("conflicting master key version")
		}
		ring.Keys[d.ID] = key
	}
	if len(ring.Keys) == 0 {
		return fmt.Errorf("no protected master key versions")
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("keyring destination must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".keyring-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(ring); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
