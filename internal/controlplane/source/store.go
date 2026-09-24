package source

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"ebof-wg-mesh/internal/controlplane/durablework"
)

type Querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var ErrLeaseLost = errors.New("source lease lost")

type SQLStore struct {
	db                 *sql.DB
	withCoordinationTx func(context.Context, func(context.Context, *sql.Tx) error) error
	services           func(context.Context, string) (Service, error)
	archives           ArchiveStore
	archiveStagingDir  string
	work               *durablework.Store
}

func NewSQLStore(db *sql.DB, withCoordinationTx func(context.Context, func(context.Context, *sql.Tx) error) error, services func(context.Context, string) (Service, error)) *SQLStore {
	return &SQLStore{db: db, withCoordinationTx: withCoordinationTx, services: services, work: durablework.NewStore(db, withCoordinationTx)}
}

// Work is the durable work queue carrying source background work. Source
// records live in the shared durable_work_items table keyed by the source
// work kinds; there is no source-specific queue table.
func (s *SQLStore) Work() *durablework.Store { return s.work }

func (s *SQLStore) SourceWorkReady() <-chan struct{} { return s.work.Ready() }

var _ Store = (*SQLStore)(nil)

func (s *SQLStore) ServiceSnapshot(ctx context.Context, serviceID string) (Service, error) {
	if s.services == nil {
		return Service{}, errors.New("source service lookup is not configured")
	}
	return s.services(ctx, serviceID)
}

func (s *SQLStore) ConfigureSourceArchives(store ArchiveStore, stagingDir string) error {
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return err
	}
	s.archives = store
	s.archiveStagingDir = stagingDir
	return nil
}

func (s *SQLStore) Archives() ArchiveStore {
	return s.archives
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}
