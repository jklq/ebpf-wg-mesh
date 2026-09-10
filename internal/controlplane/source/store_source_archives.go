package source

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"
)

// SnapshotMetadata describes a snapshot archive available for streaming.
type SnapshotMetadata struct {
	ID               string
	Digest           string
	ArchiveSizeBytes int64
}

var (
	// ErrSnapshotNotFound marks a missing snapshot row.
	ErrSnapshotNotFound = errors.New("source snapshot not found")
	// ErrSnapshotNotReady marks a snapshot that exists but cannot be served yet.
	ErrSnapshotNotReady = errors.New("source snapshot not ready")
	// ErrSnapshotTooLarge marks a snapshot over the compressed size limit.
	ErrSnapshotTooLarge = errors.New("source snapshot exceeds size limit")
	// ErrSnapshotCorrupt marks a digest mismatch, including mid-stream.
	ErrSnapshotCorrupt = errors.New("source snapshot digest verification failed")
	// ErrSnapshotUnavailable marks a snapshot whose archive object is missing.
	ErrSnapshotUnavailable = errors.New("source snapshot archive is unavailable")
)

// SnapshotService opens digest-verified snapshot archives.
type SnapshotService interface {
	OpenSnapshotArchive(context.Context, string) (SnapshotMetadata, io.Reader, error)
}

var _ SnapshotService = (*SQLStore)(nil)

// OpenSnapshotArchive resolves a snapshot and returns its metadata with a
// bounded, digest-verifying archive reader.
func (s *SQLStore) OpenSnapshotArchive(ctx context.Context, snapshotID string) (SnapshotMetadata, io.Reader, error) {
	if strings.TrimSpace(snapshotID) == "" {
		return SnapshotMetadata{}, nil, errors.New("snapshot id is required")
	}
	snapshot, err := s.SourceSnapshotByID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotMetadata{}, nil, ErrSnapshotNotFound
		}
		return SnapshotMetadata{}, nil, err
	}
	if err := EnsureReadySnapshot(snapshot); err != nil {
		return SnapshotMetadata{}, nil, fmt.Errorf("%w: %v", ErrSnapshotNotReady, err)
	}
	if snapshot.ArchiveSizeBytes > MaxArchiveCompressedBytes {
		return SnapshotMetadata{}, nil, fmt.Errorf("%w: %d bytes", ErrSnapshotTooLarge, snapshot.ArchiveSizeBytes)
	}
	if !strings.HasPrefix(snapshot.Digest, "sha256:") || len(snapshot.Digest) != len("sha256:")+sha256.Size*2 {
		return SnapshotMetadata{}, nil, fmt.Errorf("%w: digest %q is invalid", ErrSnapshotCorrupt, snapshot.Digest)
	}
	if snapshot.ObjectKey == "" || s.archives == nil {
		return SnapshotMetadata{}, nil, ErrSnapshotUnavailable
	}
	metadata := SnapshotMetadata{ID: snapshot.ID, Digest: snapshot.Digest, ArchiveSizeBytes: snapshot.ArchiveSizeBytes}
	return metadata, &snapshotArchiveReader{ctx: ctx, archives: s.archives, objectKey: snapshot.ObjectKey, snapshot: metadata, hash: sha256.New()}, nil
}

type snapshotArchiveReader struct {
	ctx       context.Context
	archives  ArchiveStore
	objectKey string
	snapshot  SnapshotMetadata
	offset    int64
	hash      hash.Hash
}

func (r *snapshotArchiveReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.snapshot.ArchiveSizeBytes - r.offset
	if remaining == 0 {
		if "sha256:"+hex.EncodeToString(r.hash.Sum(nil)) != r.snapshot.Digest {
			return 0, ErrSnapshotCorrupt
		}
		return 0, io.EOF
	}
	limit := len(p)
	if int64(limit) > remaining {
		limit = int(remaining)
	}
	chunk, err := r.archives.ReadRange(r.ctx, r.objectKey, r.offset, limit)
	if err != nil {
		return 0, err
	}
	if len(chunk) == 0 || len(chunk) > limit {
		return 0, fmt.Errorf("%w: source snapshot archive changed while streaming", ErrSnapshotCorrupt)
	}
	n := copy(p, chunk)
	r.hash.Write(chunk)
	r.offset += int64(n)
	return n, nil
}

func (s *SQLStore) StoreSourceArchive(ctx context.Context, archive []byte) (string, string, error) {
	if s.archives == nil {
		return "", "", errors.New("source archive store is not configured")
	}
	digest := ArchiveDigest(archive)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		return "", "", err
	}
	if err := s.archives.Put(ctx, key, archive); err != nil {
		return "", "", fmt.Errorf("store source archive object: %w", err)
	}
	return digest, key, nil
}

func (s *SQLStore) PruneSourceArchives(ctx context.Context, cutoff time.Time) (int, error) {
	if s.archives == nil {
		return 0, errors.New("source archive store is not configured")
	}
	var deletedKeys []string
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		deletedKeys = nil
		rows, err := tx.QueryContext(ctx,
			`SELECT ss.id, ss.object_key
			   FROM source_snapshots ss
			  WHERE ss.created_at < $1
			    AND ss.object_key <> ''
			    AND NOT EXISTS (
			        SELECT 1 FROM build_runs b
			         WHERE b.source_snapshot_id = ss.id
			           AND b.state IN ('queued', 'running')
			    )
			  ORDER BY ss.created_at, ss.id
			  LIMIT 1000
			  FOR UPDATE OF ss`,
			cutoff,
		)
		if err != nil {
			return err
		}
		type candidate struct{ id, key string }
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.key); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range candidates {
			if _, err := tx.ExecContext(ctx, `DELETE FROM source_snapshots WHERE id = $1`, item.id); err != nil {
				return err
			}
			var remaining int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM source_snapshots WHERE object_key = $1`, item.key).Scan(&remaining); err != nil {
				return err
			}
			if remaining == 0 {
				deletedKeys = append(deletedKeys, item.key)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, key := range deletedKeys {
		if err := s.archives.Delete(ctx, key); err != nil {
			return 0, err
		}
	}
	return len(deletedKeys), nil
}

func (s *SQLStore) SourceStorageReady() bool {
	store, ok := s.archives.(interface{ Ready() bool })
	return ok && store.Ready()
}
