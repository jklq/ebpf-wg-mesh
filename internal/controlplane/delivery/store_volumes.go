package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *persistence) requireVolumeQuerier(ctx context.Context, q ServiceQueryer, environmentID, volumeName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM volumes WHERE environment_id = $1 AND name = $2`, environmentID, volumeName).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", ErrVolumeNotFound, volumeName)
	default:
		return err
	}
}
