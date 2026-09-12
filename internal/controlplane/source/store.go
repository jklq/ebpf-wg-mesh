package source

import (
	"context"
	"database/sql"
	"errors"
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
	workReady          chan struct{}
}

func NewSQLStore(db *sql.DB, withCoordinationTx func(context.Context, func(context.Context, *sql.Tx) error) error, services func(context.Context, string) (Service, error)) *SQLStore {
	return &SQLStore{db: db, withCoordinationTx: withCoordinationTx, services: services, workReady: make(chan struct{}, 1)}
}

func (s *SQLStore) SourceWorkReady() <-chan struct{} { return s.workReady }

func (s *SQLStore) signalSourceWork() {
	select {
	case s.workReady <- struct{}{}:
	default:
	}
}

var _ Store = (*SQLStore)(nil)

func (s *SQLStore) ServiceSnapshot(ctx context.Context, serviceID string) (Service, error) {
	if s.services == nil {
		return Service{}, errors.New("source service lookup is not configured")
	}
	return s.services(ctx, serviceID)
}

func (s *SQLStore) ConfigureSourceArchives(store ArchiveStore) {
	s.archives = store
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
