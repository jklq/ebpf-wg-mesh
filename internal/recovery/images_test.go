package recovery

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ociFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	blob := func(b []byte, media string) descriptor {
		d := descriptor{Digest: Digest(b), Size: int64(len(b)), MediaType: media}
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:")), b, 0600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	layer := blob([]byte("shared layer"), "application/vnd.oci.image.layer.v1.tar")
	index := imageManifest{SchemaVersion: 2}
	var configPath string
	for _, arch := range []string{"amd64", "arm64"} {
		config := blob(jsonBytes(map[string]string{"architecture": arch, "os": "linux"}), "application/vnd.oci.image.config.v1+json")
		configPath = filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(config.Digest, "sha256:"))
		m := imageManifest{SchemaVersion: 2, Config: config, Layers: []descriptor{layer}}
		index.Manifests = append(index.Manifests, blob(jsonBytes(m), "application/vnd.oci.image.manifest.v1+json"))
	}
	root := blob(jsonBytes(index), "application/vnd.oci.image.index.v1+json")
	if err := os.WriteFile(filepath.Join(dir, "index.json"), jsonBytes(imageManifest{SchemaVersion: 2, Manifests: []descriptor{root}}), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, root.Digest, configPath
}

func archiveOCI(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if err := w.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Size: int64(len(b)), Mode: 0600}); err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImageInventoryCoversAllArchitecturesAndBlobs(t *testing.T) {
	dir, root, config := ociFixture(t)
	inventory, err := InspectOCI(dir, root)
	if err != nil || len(inventory) != 6 {
		t.Fatal(inventory, err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectOCI(dir, root); err == nil {
		t.Fatal("missing architecture configuration accepted")
	}
}

func TestMissingImageBlobPreventsCompletePoint(t *testing.T) {
	s, _, p := fixture(t, time.Now().Add(-time.Minute))
	dir, root, config := ociFixture(t)
	inventory, err := InspectOCI(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	r := Requirement{Kind: "image", ID: "registry.example/mesh/app@" + root, Digest: root}
	p.Snapshot.Requirements = append(p.Snapshot.Requirements, r)
	if _, err := s.Protect(context.Background(), r, archiveOCI(t, dir), false, inventory); err != nil {
		t.Fatal(err)
	}
	_, report, err := s.Publish(context.Background(), p)
	if err == nil || report.Complete {
		t.Fatal("partial OCI export produced a complete point", report)
	}
}

func TestCompleteImagePointAndUnsafeArchive(t *testing.T) {
	s, _, p := fixture(t, time.Now().Add(-time.Minute))
	dir, root, _ := ociFixture(t)
	inventory, err := InspectOCI(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	r := Requirement{Kind: "image", ID: "registry.example/mesh/app@" + root, Digest: root}
	p.Snapshot.Requirements = append(p.Snapshot.Requirements, r)
	if _, err := s.Protect(context.Background(), r, archiveOCI(t, dir), false, inventory); err != nil {
		t.Fatal(err)
	}
	if _, report, err := s.Publish(context.Background(), p); err != nil || !report.Complete {
		t.Fatal(report, err)
	}
	for _, header := range []tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}, {Name: "blobs/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}} {
		path := filepath.Join(t.TempDir(), "bad.tar")
		f, _ := os.Create(path)
		w := tar.NewWriter(f)
		if err := w.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		w.Close()
		f.Close()
		if err := extractOCI(path, t.TempDir()); err == nil {
			t.Fatal("unsafe archive accepted", header)
		}
	}
}
