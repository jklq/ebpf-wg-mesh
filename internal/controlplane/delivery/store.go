package delivery

import (
	"context"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func (s *persistence) currentDesiredRevisionForAgent(ctx context.Context, agentID string) (int64, error) {
	var value int64
	if err := s.db.QueryRowContext(ctx, `SELECT desired_revision FROM agents WHERE id = $1`, agentID).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}
