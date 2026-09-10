package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
)

const DefaultRetainEntries = 1024

type SnapshotFence func(context.Context, *sql.Tx) error

func (s *Store) CompactSnapshot(ctx context.Context, retain int64, fence SnapshotFence) (int64, error) {
	if retain < 0 {
		retain = 0
	}
	var target int64
	err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		if fence != nil {
			if err := fence(ctx, tx); err != nil {
				return err
			}
		}
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1 FOR UPDATE`, s.clusterID).Scan(&head); err != nil {
			return err
		}
		watermark, err := snapshotWatermark(ctx, tx, s.clusterID)
		if err != nil {
			return err
		}
		target = head - retain
		if target <= watermark {
			target = watermark
			return nil
		}
		state, err := loadSnapshotState(ctx, tx, s.clusterID)
		if err != nil {
			return err
		}
		state.ClusterID = s.clusterID
		if state.LogIndex < watermark {
			state.LogIndex = watermark
		}
		state, err = replay(ctx, tx, state, target)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cluster_journal_snapshots(cluster_id, log_index, state, created_at)
			VALUES ($1, $2, $3, statement_timestamp())
			ON CONFLICT(cluster_id) DO UPDATE
			SET log_index = excluded.log_index, state = excluded.state, created_at = excluded.created_at`,
			s.clusterID, target, raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cluster_journal WHERE cluster_id = $1 AND log_index <= $2`, s.clusterID, target); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return target, nil
}

func snapshotWatermark(ctx context.Context, tx *sql.Tx, clusterID string) (int64, error) {
	var watermark int64
	err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_snapshots WHERE cluster_id = $1`, clusterID).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return watermark, err
}

func loadSnapshotState(ctx context.Context, tx *sql.Tx, clusterID string) (DurableState, error) {
	var watermark int64
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT log_index, state FROM cluster_journal_snapshots WHERE cluster_id = $1`, clusterID).Scan(&watermark, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DurableState{ClusterID: clusterID}, nil
	}
	if err != nil {
		return DurableState{}, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return DurableState{}, err
	}
	var state DurableState
	if err := json.Unmarshal(compact.Bytes(), &state); err != nil {
		return DurableState{}, err
	}
	state.ClusterID = clusterID
	state.LogIndex = watermark
	return state, nil
}

func (s *Store) resumeState(ctx context.Context, tx *sql.Tx) (DurableState, error) {
	watermark, err := snapshotWatermark(ctx, tx, s.clusterID)
	if err != nil {
		return DurableState{}, err
	}
	if watermark > s.state.LogIndex {
		return loadSnapshotState(ctx, tx, s.clusterID)
	}
	return s.state, nil
}
