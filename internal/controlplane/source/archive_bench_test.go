package source

import (
	"bytes"
	"context"
	"testing"
)

// Compare a verified 1 MiB range read. The fake S3 HTTP endpoint runs on loopback:
// this includes HTTP/signing/buffering, but excludes WAN and remote server cost.
func BenchmarkArchiveRead(b *testing.B) {
	for _, kind := range []string{"file", "s3"} {
		b.Run(kind, func(b *testing.B) {
			var store ArchiveStore
			var err error
			if kind == "file" {
				store, err = NewFileArchiveStore(b.TempDir())
			} else {
				store, err = NewS3ArchiveStore(newFakeS3(b, "bench-bucket").config())
			}
			if err != nil {
				b.Fatal(err)
			}
			payload := bytes.Repeat([]byte("a"), 1<<20)
			digest := ArchiveDigest(payload)
			key, err := ArchiveObjectKey(digest)
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.ReadRange(ctx, key, 0, len(payload)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
