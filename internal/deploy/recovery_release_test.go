package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ebof-wg-mesh/internal/recovery"
	"go.yaml.in/yaml/v3"
)

type releaseStorage struct {
	recovery.Storage
	body []byte
}

func (s releaseStorage) Get(_ context.Context, _ recovery.Object, path string) error {
	return os.WriteFile(path, s.body, 0600)
}

func TestRecoveryReleaseMatchesSchemaAndCompleteExecutableInventory(t *testing.T) {
	_, bundle, _ := fixture(1)
	data, err := yaml.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r := recovery.Requirement{Kind: "release", ID: bundle.ID, Digest: recovery.Digest(data)}
	needs, _, err := releaseRequirements(recovery.Config{Files: []recovery.File{{Requirement: r, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := recovery.Snapshot{Schema: bundle.Schema, ConsoleSchema: bundle.ConsoleSchema, Requirements: []recovery.Requirement{r}}
	for _, need := range needs {
		snapshot.Requirements = append(snapshot.Requirements, need)
	}
	dependencies := []recovery.Dependency{{Kind: "release", ID: bundle.ID, Objects: []recovery.Object{{Digest: r.Digest, Size: int64(len(data))}}}}
	s := recovery.Service{Storage: releaseStorage{body: data}}
	if err := verifyReleaseInventory(context.Background(), s, snapshot, dependencies); err != nil {
		t.Fatal(err)
	}
	snapshot.Schema++
	if err := verifyReleaseInventory(context.Background(), s, snapshot, dependencies); err == nil {
		t.Fatal("post-conversion database accepted only the previous schema's release")
	}
	snapshot.Schema--
	snapshot.Requirements = snapshot.Requirements[:1]
	if err := verifyReleaseInventory(context.Background(), s, snapshot, dependencies); err == nil {
		t.Fatal("release executable omitted from timestamped inventory")
	}
}
