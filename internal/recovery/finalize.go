package recovery

import (
	"context"
	"database/sql"
	"fmt"
)

func VerifyFinishedRelease(ctx context.Context, db *sql.DB, installation, release string) error {
	var total, current int
	if err := db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE release=$2) FROM platform_recovery.public.installations WHERE installation=$1`, installation, release).Scan(&total, &current); err != nil {
		return err
	}
	if total != 1 || current != 1 {
		return fmt.Errorf("installer inventory has not finalized the selected release")
	}
	return nil
}
