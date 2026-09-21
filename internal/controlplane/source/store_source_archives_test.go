package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
)

type stubArchiveStore struct {
	ArchiveStore
	readRange func(ctx context.Context, key string, offset int64, limit int) ([]byte, error)
}

func (s stubArchiveStore) ReadRange(ctx context.Context, key string, offset int64, limit int) ([]byte, error) {
	return s.readRange(ctx, key, offset, limit)
}

func TestSnapshotArchiveReaderStreamsAndVerifiesDigest(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("snapshot-chunk-data"), 4096)
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	store := stubArchiveStore{readRange: func(_ context.Context, _ string, offset int64, limit int) ([]byte, error) {
		if offset >= int64(len(payload)) {
			return []byte{}, nil
		}
		end := offset + int64(limit)
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		return payload[offset:end], nil
	}}
	reader := &snapshotArchiveReader{
		ctx:       context.Background(),
		archives:  store,
		objectKey: key,
		snapshot:  SnapshotMetadata{ID: "snap-1", Digest: digest, ArchiveSizeBytes: int64(len(payload))},
		hash:      sha256.New(),
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("streamed snapshot does not match stored content")
	}
}

func TestSnapshotArchiveReaderMapsStoreFailures(t *testing.T) {
	t.Parallel()
	payload := []byte("snapshot-bytes")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	newReader := func(store ArchiveStore) *snapshotArchiveReader {
		return &snapshotArchiveReader{
			ctx:       context.Background(),
			archives:  store,
			objectKey: key,
			snapshot:  SnapshotMetadata{ID: "snap-1", Digest: digest, ArchiveSizeBytes: int64(len(payload))},
			hash:      sha256.New(),
		}
	}

	t.Run("transient", func(t *testing.T) {
		t.Parallel()
		reader := newReader(stubArchiveStore{readRange: func(context.Context, string, int64, int) ([]byte, error) {
			return nil, errors.Join(ErrArchiveTransient, errors.New("status 503"))
		}})
		if _, err := io.ReadAll(reader); !errors.Is(err, ErrSnapshotTransient) {
			t.Fatalf("error = %v, want transient", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		reader := newReader(stubArchiveStore{readRange: func(context.Context, string, int64, int) ([]byte, error) {
			return nil, errors.Join(ErrArchiveNotFound, errors.New(key))
		}})
		if _, err := io.ReadAll(reader); !errors.Is(err, ErrSnapshotMissing) {
			t.Fatalf("error = %v, want missing", err)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		t.Parallel()
		reader := newReader(stubArchiveStore{readRange: func(_ context.Context, _ string, offset int64, limit int) ([]byte, error) {
			if offset > 0 {
				return []byte{}, nil
			}
			corrupt := append([]byte(nil), payload...)
			corrupt[0] ^= 0xff
			return corrupt[:limit], nil
		}})
		if _, err := io.ReadAll(reader); !errors.Is(err, ErrSnapshotCorrupt) {
			t.Fatalf("error = %v, want corrupt", err)
		}
	})

	t.Run("short stream", func(t *testing.T) {
		t.Parallel()
		reader := newReader(stubArchiveStore{readRange: func(context.Context, string, int64, int) ([]byte, error) {
			return []byte{}, nil
		}})
		if _, err := io.ReadAll(reader); !errors.Is(err, ErrSnapshotCorrupt) {
			t.Fatalf("error = %v, want corrupt", err)
		}
	})
}
