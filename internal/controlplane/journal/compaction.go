package journal

import (
	"context"
	"database/sql"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
)

const (
	DefaultRetainEntries          = 1024
	InternalReceiptLifetime       = time.Hour
	CallerProvidedReceiptLifetime = 7 * 24 * time.Hour
)

type CompactionFence func(context.Context, *sql.Tx) error

// Compact bounds the replay tail and command receipts without materializing a
// second copy of product state. A reader that falls behind compacted_index
// rebuilds directly from the authoritative normalized tables.
func (s *Store) Compact(ctx context.Context, retain int64, fence CompactionFence) (int64, error) {
	if retain < 0 {
		retain = 0
	}
	var compacted int64
	err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		if fence != nil {
			if err := fence(ctx, tx); err != nil {
				return err
			}
		}
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index, compacted_index FROM cluster_journal_heads WHERE cluster_id = $1 FOR UPDATE`, s.clusterID).Scan(&head, &compacted); err != nil {
			return err
		}
		target := head - retain
		// Normal operation truncates in batches instead of turning each new
		// command beyond the retained tail into a compaction transaction. Tests
		// and operator-forced compaction use retain == 0 to compact immediately.
		if retain > 0 && target-compacted < retain {
			target = compacted
		}
		if target > compacted {
			if _, err := tx.ExecContext(ctx, `DELETE FROM cluster_journal WHERE cluster_id = $1 AND log_index <= $2`, s.clusterID, target); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE cluster_journal_heads SET compacted_index = $2 WHERE cluster_id = $1`, s.clusterID, target); err != nil {
				return err
			}
			compacted = target
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM cluster_journal_receipts
			WHERE cluster_id = $1 AND expires_at <= statement_timestamp()`, s.clusterID)
		return err
	})
	return compacted, err
}
