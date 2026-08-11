package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

const (
	maxSourceArchiveExpandedBytes = 1 << 30
	maxSourceArchiveFileBytes     = 256 << 20
	maxSourceArchiveEntries       = 100_000
)

func validateSourceArchive(archiveTGZ []byte) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveTGZ))
	if err != nil {
		return fmt.Errorf("open source archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var totalBytes int64
	for entries := 1; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		if entries > maxSourceArchiveEntries {
			return errors.New("source archive contains too many entries")
		}
		if hdr.Size < 0 || hdr.Size > maxSourceArchiveFileBytes {
			return fmt.Errorf("source archive entry %q exceeds file size limit", hdr.Name)
		}
		if hdr.Size > maxSourceArchiveExpandedBytes-totalBytes {
			return errors.New("source archive exceeds expanded size limit")
		}
		totalBytes += hdr.Size
		if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
			return fmt.Errorf("read source archive entry %q: %w", hdr.Name, err)
		}
	}
}
