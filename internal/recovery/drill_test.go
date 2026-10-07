package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/controlplane/source"
)

func TestRestoreRequiresArchivesAvailableAtTheirOriginalKeys(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "source")
	b := []byte("source archive")
	digest := Digest(b)
	key, err := source.ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	p := Point{Dependencies: []Dependency{{Kind: "source", ID: key, Digest: digest, Objects: []Object{{Size: int64(len(b))}}}}}
	if err := checkRestoredSources(p, directory, workspace); err == nil {
		t.Fatal("missing restored archive accepted")
	}
	path := filepath.Join(directory, key)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkRestoredSources(p, directory, workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(b))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkRestoredSources(p, directory, workspace); err == nil {
		t.Fatal("corrupt restored archive accepted")
	}
}
