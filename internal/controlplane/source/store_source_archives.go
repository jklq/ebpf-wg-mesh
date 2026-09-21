package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"
)

type SnapshotMetadata struct {
	ID               string
	Digest           string
	ArchiveSizeBytes int64
}

var (
	ErrSnapshotNotFound = errors.New("source snapshot not found")
	// ErrSnapshotNotReady marks a snapshot that exists but cannot be served yet.
	ErrSnapshotNotReady    = errors.New("source snapshot not ready")
	ErrSnapshotTooLarge    = errors.New("source snapshot exceeds size limit")
	ErrSnapshotCorrupt     = errors.New("source snapshot digest verification failed")
	ErrSnapshotMissing     = errors.New("source snapshot object is missing")
	ErrSnapshotTransient   = errors.New("source snapshot storage is temporarily unavailable")
	ErrSnapshotUnavailable = errors.New("source snapshot archive is unavailable")
)

type SnapshotService interface {
	OpenSnapshotArchive(context.Context, string) (SnapshotMetadata, io.Reader, error)
}

var _ SnapshotService = (*SQLStore)(nil)

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
	if stat, err := s.archives.Stat(ctx, snapshot.ObjectKey); err != nil {
		return SnapshotMetadata{}, nil, mapArchiveReadError(err)
	} else if stat.Size != snapshot.ArchiveSizeBytes || stat.Digest != snapshot.Digest {
		return SnapshotMetadata{}, nil, fmt.Errorf("%w: stored object metadata does not match snapshot", ErrSnapshotCorrupt)
	}
	metadata := SnapshotMetadata{ID: snapshot.ID, Digest: snapshot.Digest, ArchiveSizeBytes: snapshot.ArchiveSizeBytes}
	return metadata, &snapshotArchiveReader{ctx: ctx, archives: s.archives, objectKey: snapshot.ObjectKey, snapshot: metadata, hash: sha256.New()}, nil
}

func mapArchiveReadError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case IsArchiveTransient(err):
		return fmt.Errorf("%w: %v", ErrSnapshotTransient, err)
	case IsArchiveNotFound(err):
		return fmt.Errorf("%w: %v", ErrSnapshotMissing, err)
	case IsArchiveCorrupt(err):
		return fmt.Errorf("%w: %v", ErrSnapshotCorrupt, err)
	default:
		return err
	}
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
		return 0, mapArchiveReadError(err)
	}
	if len(chunk) == 0 || len(chunk) > limit {
		return 0, fmt.Errorf("%w: source snapshot archive changed while streaming", ErrSnapshotCorrupt)
	}
	n := copy(p, chunk)
	r.hash.Write(chunk)
	r.offset += int64(n)
	return n, nil
}

const (
	ArchiveObjectStateReady    = "ready"
	ArchiveObjectStateDeleting = "deleting"
)

func (s *SQLStore) StoreSourceArchive(ctx context.Context, archive []byte) (string, string, error) {
	if s.archives == nil {
		return "", "", errors.New("source archive store is not configured")
	}
	if len(archive) == 0 {
		return "", "", fmt.Errorf("%w: source archive is empty", ErrSnapshotCorrupt)
	}
	if len(archive) > MaxArchiveCompressedBytes {
		return "", "", fmt.Errorf("%w: %d bytes", ErrSnapshotTooLarge, len(archive))
	}
	digest := ArchiveDigest(archive)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		return "", "", err
	}
	put := func() error {
		return s.archives.Put(ctx, key, bytes.NewReader(archive), int64(len(archive)), digest)
	}
	if err := s.commitStagedArchive(ctx, key, digest, int64(len(archive)), put); err != nil {
		return "", "", err
	}
	return digest, key, nil
}

func (s *SQLStore) commitStagedArchive(ctx context.Context, key, digest string, size int64, put func() error) error {
	if err := s.upsertSourceArchiveObject(ctx, key, digest, size); err != nil {
		return err
	}
	if err := put(); err != nil {
		return fmt.Errorf("store source archive object: %w", err)
	}
	return s.verifyStoredArchiveObject(ctx, key, digest, size, put)
}

func (s *SQLStore) StoreSourceArchiveFromReader(ctx context.Context, body io.Reader, size int64) (string, string, int64, error) {
	if s.archives == nil {
		return "", "", 0, errors.New("source archive store is not configured")
	}
	if body == nil {
		return "", "", 0, errors.New("source archive body is required")
	}
	if size == 0 {
		return "", "", 0, fmt.Errorf("%w: source archive is empty", ErrSnapshotCorrupt)
	}
	if size > MaxArchiveCompressedBytes {
		return "", "", 0, fmt.Errorf("%w: %d bytes", ErrSnapshotTooLarge, size)
	}
	staged, err := os.CreateTemp("", "source-archive-stage-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("stage source archive: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	hash := sha256.New()
	limit := int64(MaxArchiveCompressedBytes) + 1
	if size > 0 {
		limit = size + 1
	}
	written, err := io.CopyN(io.MultiWriter(staged, hash), body, limit)
	if err != nil && !errors.Is(err, io.EOF) {
		staged.Close()
		return "", "", 0, fmt.Errorf("stage source archive: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		staged.Close()
		return "", "", 0, ctxErr
	}
	if size < 0 {
		size = written
	}
	if written != size {
		staged.Close()
		return "", "", 0, fmt.Errorf("%w: declared size %d but body yielded %d bytes", ErrSnapshotCorrupt, size, written)
	}
	if size <= 0 {
		staged.Close()
		return "", "", 0, fmt.Errorf("%w: source archive is empty", ErrSnapshotCorrupt)
	}
	if size > MaxArchiveCompressedBytes {
		staged.Close()
		return "", "", 0, fmt.Errorf("%w: %d bytes", ErrSnapshotTooLarge, size)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		staged.Close()
		return "", "", 0, fmt.Errorf("stage source archive: %w", err)
	}
	if err := ValidateArchiveStream(staged); err != nil {
		staged.Close()
		return "", "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		staged.Close()
		return "", "", 0, err
	}
	put := func() error {
		if _, err := staged.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return s.archives.Put(ctx, key, staged, size, digest)
	}
	if err := s.commitStagedArchive(ctx, key, digest, size, put); err != nil {
		staged.Close()
		return "", "", 0, err
	}
	staged.Close()
	return digest, key, size, nil
}

func (s *SQLStore) verifyStoredArchiveObject(ctx context.Context, key, digest string, size int64, repair func() error) error {
	stat, err := s.archives.Stat(ctx, key)
	if err != nil {
		if !IsArchiveNotFound(err) || repair == nil {
			return mapArchiveReadError(err)
		}
		if err := repair(); err != nil {
			return fmt.Errorf("store source archive object: %w", err)
		}
		stat, err = s.archives.Stat(ctx, key)
		if err != nil {
			return mapArchiveReadError(err)
		}
	}
	if stat.Size != size || stat.Digest != digest {
		return fmt.Errorf("%w: stored object metadata does not match snapshot", ErrSnapshotCorrupt)
	}
	return nil
}

func (s *SQLStore) upsertSourceArchiveObject(ctx context.Context, key, digest string, size int64) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return upsertSourceArchiveObjectTx(ctx, tx, key, digest, size)
	})
}

func upsertSourceArchiveObjectTx(ctx context.Context, tx *sql.Tx, key, digest string, size int64) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(digest) == "" || size <= 0 {
		return errors.New("source archive object identity is required")
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO source_archive_objects(object_key, digest, size_bytes, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, statement_timestamp(), statement_timestamp())
		 ON CONFLICT(object_key) DO UPDATE
		    SET digest = excluded.digest,
		        size_bytes = excluded.size_bytes,
		        state = excluded.state,
		        updated_at = statement_timestamp()`,
		key, digest, size, ArchiveObjectStateReady,
	)
	return err
}

func (s *SQLStore) PruneSourceArchives(ctx context.Context, cutoff time.Time) (int, error) {
	if s.archives == nil {
		return 0, errors.New("source archive store is not configured")
	}
	if err := s.pruneExpiredSnapshots(ctx, cutoff); err != nil {
		return 0, err
	}
	return s.collectUnreferencedArchiveObjects(ctx, cutoff)
}

func (s *SQLStore) pruneExpiredSnapshots(ctx context.Context, cutoff time.Time) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT ss.id
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
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM source_snapshots WHERE id = $1`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLStore) collectUnreferencedArchiveObjects(ctx context.Context, cutoff time.Time) (int, error) {
	claimed, err := s.claimArchiveObjectsForCollection(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	collected := 0
	for _, key := range claimed {
		proceed, err := s.verifyArchiveObjectUnreferenced(ctx, key, cutoff)
		if err != nil {
			return collected, err
		}
		if !proceed {
			continue
		}
		if err := s.archives.Delete(ctx, key); err != nil {
			_ = s.releaseArchiveObjectClaim(ctx, key)
			return collected, fmt.Errorf("delete source archive object: %w", err)
		}
		deleted, err := s.deleteArchiveObjectRow(ctx, key, cutoff)
		if err != nil {
			return collected, err
		}
		if deleted {
			collected++
		}
	}
	return collected, nil
}

func (s *SQLStore) claimArchiveObjectsForCollection(ctx context.Context, cutoff time.Time) ([]string, error) {
	var claimed []string
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		claimed = nil
		rows, err := tx.QueryContext(ctx,
			`SELECT o.object_key
			   FROM source_archive_objects o
			  WHERE o.state = $1
			    AND o.updated_at < $2
			    AND NOT EXISTS (
			        SELECT 1 FROM source_snapshots ss WHERE ss.object_key = o.object_key
			    )
			    AND NOT EXISTS (
			        SELECT 1 FROM build_runs b
			         WHERE b.source_snapshot_digest = o.digest
			           AND b.state IN ('queued', 'running')
			    )
			  ORDER BY o.updated_at, o.object_key
			  LIMIT 100
			  FOR UPDATE OF o`,
			ArchiveObjectStateReady, cutoff,
		)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			claimed = append(claimed, key)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, key := range claimed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE source_archive_objects SET state = $1 WHERE object_key = $2 AND state = $3`,
				ArchiveObjectStateDeleting, key, ArchiveObjectStateReady,
			); err != nil {
				return err
			}
		}
		return nil
	})
	return claimed, err
}

func (s *SQLStore) verifyArchiveObjectUnreferenced(ctx context.Context, key string, cutoff time.Time) (bool, error) {
	proceed := false
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var state string
		var updatedAt time.Time
		if err := tx.QueryRowContext(ctx,
			`SELECT state, updated_at FROM source_archive_objects WHERE object_key = $1`,
			key,
		).Scan(&state, &updatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				proceed = false
				return nil
			}
			return err
		}
		if state != ArchiveObjectStateDeleting || !updatedAt.Before(cutoff) {
			proceed = false
			return nil
		}
		var referenced int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM source_snapshots WHERE object_key = $1`,
			key,
		).Scan(&referenced); err != nil {
			return err
		}
		if referenced > 0 {
			proceed = false
			_, err := tx.ExecContext(ctx,
				`UPDATE source_archive_objects SET state = $1 WHERE object_key = $2`,
				ArchiveObjectStateReady, key,
			)
			return err
		}
		proceed = true
		return nil
	})
	return proceed, err
}

func (s *SQLStore) releaseArchiveObjectClaim(ctx context.Context, key string) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE source_archive_objects SET state = $1 WHERE object_key = $2 AND state = $3`,
			ArchiveObjectStateReady, key, ArchiveObjectStateDeleting,
		)
		return err
	})
}

func (s *SQLStore) deleteArchiveObjectRow(ctx context.Context, key string, cutoff time.Time) (bool, error) {
	deleted := false
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM source_archive_objects o
			  WHERE o.object_key = $1
			    AND o.state = $2
			    AND o.updated_at < $3
			    AND NOT EXISTS (
			        SELECT 1 FROM source_snapshots ss WHERE ss.object_key = o.object_key
			    )
			    AND NOT EXISTS (
			        SELECT 1 FROM build_runs b
			         WHERE b.source_snapshot_digest = o.digest
			           AND b.state IN ('queued', 'running')
			    )`,
			key, ArchiveObjectStateDeleting, cutoff,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = rows > 0
		return nil
	})
	return deleted, err
}

func (s *SQLStore) SourceStorageReady() bool {
	store, ok := s.archives.(interface{ Ready() bool })
	return ok && store.Ready()
}
