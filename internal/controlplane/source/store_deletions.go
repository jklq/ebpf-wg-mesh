package source

import (
	"context"
	"database/sql"
)

// DeletePendingServiceWorkTx drops queued work for a service being deleted so
// no new builds, revisions, or syncs start during the grace period. Items
// already leased finish into guards that drop tombstoned services. The
// resource identity matches SourceSpecChangedParams and SourceResyncParams.
func (s *SQLStore) DeletePendingServiceWorkTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	return s.work.CancelPendingForResourceTx(ctx, tx, "service", serviceID)
}
