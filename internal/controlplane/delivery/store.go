package delivery

import (
	"context"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func (s *persistence) readAuthorityEpoch(ctx context.Context) (uint64, error) {
	var epoch uint64
	err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch)
	return epoch, err
}
