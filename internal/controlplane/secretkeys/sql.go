package secretkeys

import (
	"context"
	"database/sql"
)

// Querier abstracts *sql.DB and *sql.Tx so stores compose inside caller
// transactions (seal alongside deployment capture, rotate alongside audit).
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
