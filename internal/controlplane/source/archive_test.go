package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func TestValidateSourceArchiveRejectsOversizedEntry(t *testing.T) {
	t.Parallel()

	var archive bytes.Buffer
	gzw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "repo/large", Mode: 0o644, Size: maxSourceArchiveFileBytes + 1}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}

	err := ValidateArchive(archive.Bytes())
	if err == nil || !strings.Contains(err.Error(), "file size limit") {
		t.Fatalf("expected file size limit error, got %v", err)
	}
}
