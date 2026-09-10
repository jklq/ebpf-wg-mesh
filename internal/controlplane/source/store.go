package source

import (
	"context"
	"database/sql"
	"errors"
)

// Querier abstracts *sql.DB and *sql.Tx for tx-scoped source queries.
type Querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ErrLeaseLost reports a lost work-item or webhook-delivery lease: another
// processor claimed the row first.
var ErrLeaseLost = errors.New("source lease lost")

// SQLStore owns source-domain persistence: bindings, revisions, snapshots,
// work items, archives, and GitHub state. It deliberately has no domain
// methods beyond row mapping; coordination lives in GitHubCoordinator and
// delivery owns deployment policy.
type SQLStore struct {
	db       *sql.DB
	withTx   func(context.Context, func(context.Context, *sql.Tx) error) error
	services func(context.Context, string) (Service, error)
	archives ArchiveStore
}

// NewSQLStore wires a SQLStore. withTx must provide the control-plane
// transaction semantics (lease fencing, retries); services adapts the
// delivery read model into the source service view.
func NewSQLStore(db *sql.DB, withTx func(context.Context, func(context.Context, *sql.Tx) error) error, services func(context.Context, string) (Service, error)) *SQLStore {
	return &SQLStore{db: db, withTx: withTx, services: services}
}

var _ Store = (*SQLStore)(nil)

// ServiceSnapshot resolves the source-relevant service identity via the
// injected delivery read-model adapter.
func (s *SQLStore) ServiceSnapshot(ctx context.Context, serviceID string) (Service, error) {
	if s.services == nil {
		return Service{}, errors.New("source service lookup is not configured")
	}
	return s.services(ctx, serviceID)
}

// ConfigureSourceArchives wires the object store for snapshot archives.
func (s *SQLStore) ConfigureSourceArchives(store ArchiveStore) {
	s.archives = store
}

// Archives exposes the configured object store, mainly for tests asserting
// archive retention.
func (s *SQLStore) Archives() ArchiveStore {
	return s.archives
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}
