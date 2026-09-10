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
	db       *sql.DB
	withTx   func(context.Context, func(context.Context, *sql.Tx) error) error
	services func(context.Context, string) (Service, error)
	archives ArchiveStore
}

func NewSQLStore(db *sql.DB, withTx func(context.Context, func(context.Context, *sql.Tx) error) error, services func(context.Context, string) (Service, error)) *SQLStore {
	return &SQLStore{db: db, withTx: withTx, services: services}
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
