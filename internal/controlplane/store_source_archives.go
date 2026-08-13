package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) ConfigureSourceArchives(store SourceArchiveStore) {
	s.sourceArchives = store
}

func (s *Store) storeSourceArchive(ctx context.Context, archive []byte) (string, string, error) {
	if s.sourceArchives == nil {
		return "", "", errors.New("source archive store is not configured")
	}
	digest := snapshotDigest(archive)
	key, err := sourceArchiveObjectKey(digest)
	if err != nil {
		return "", "", err
	}
	if err := s.sourceArchives.Put(ctx, key, archive); err != nil {
		return "", "", fmt.Errorf("store source archive object: %w", err)
	}
	return digest, key, nil
}

func (s *Store) loadSourceArchive(ctx context.Context, snapshot sourceSnapshotRecord) ([]byte, error) {
	if s.sourceArchives == nil {
		return nil, errors.New("source archive store is not configured")
	}
	if snapshot.ObjectKey == "" {
		return nil, fmt.Errorf("snapshot %s has no source archive object", snapshot.ID)
	}
	archive, err := s.sourceArchives.Get(ctx, snapshot.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("load source archive object %s: %w", snapshot.ObjectKey, err)
	}
	if got := snapshotDigest(archive); got != snapshot.Digest {
		return nil, fmt.Errorf("source archive digest mismatch: got %s want %s", got, snapshot.Digest)
	}
	return archive, nil
}

func (s *Store) pruneSourceArchives(ctx context.Context, cutoff time.Time) (int, error) {
	if s.sourceArchives == nil {
		return 0, errors.New("source archive store is not configured")
	}
	var deletedKeys []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
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
		if err := s.sourceArchives.Delete(ctx, key); err != nil {
			return 0, err
		}
	}
	return len(deletedKeys), nil
}
