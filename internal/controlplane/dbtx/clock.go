package dbtx

import (
	"context"
	"database/sql"
	"time"
)

func DatabaseTime(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (time.Time, error) {
	var now time.Time
	err := q.QueryRowContext(ctx, `SELECT statement_timestamp()`).Scan(&now)
	return now.UTC(), err
}
