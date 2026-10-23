package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractSourceSnapshotRejectsChainedSymlinkEscapes(t *testing.T) {
	for _, attack := range []struct {
		name string
		tail []tar.Header
	}{
		{name: "escaping link"},
		{name: "write outside repository", tail: []tar.Header{
			{Name: "repo/escape/planted", Typeflag: tar.TypeSymlink, Linkname: "."},
		}},
		{name: "read outside repository via hardlink", tail: []tar.Header{
			{Name: "repo/stolen", Typeflag: tar.TypeLink, Linkname: "repo/escape/secret"},
		}},
	} {
		t.Run(attack.name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			outside := filepath.Join(base, "outside")
			for _, dir := range []string{repo, outside} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			headers := append([]tar.Header{
				{Name: "repo/alias", Typeflag: tar.TypeSymlink, Linkname: "."},
				// Lexically this is "outside", but alias/.. resolves above repo.
				{Name: "repo/escape", Typeflag: tar.TypeSymlink, Linkname: "alias/../outside"},
			}, attack.tail...)
			for _, header := range headers {
				if err := tw.WriteHeader(&header); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractSourceSnapshot(repo, archive.Bytes()); err == nil {
				t.Error("accepted a chained symlink escape")
			}
			if _, err := os.Lstat(filepath.Join(outside, "planted")); !os.IsNotExist(err) {
				t.Errorf("archive wrote outside repository: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(repo, "stolen")); !os.IsNotExist(err) {
				t.Errorf("archive hardlinked a host file into repository: %v", err)
			}
		})
	}
}
