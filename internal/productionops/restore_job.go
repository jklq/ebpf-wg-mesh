package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The default SHOW JOBS description is abbreviated and its completed-job
// window is too short for interrupted recovery. Inspect the complete native
// record, including unsuccessful jobs, before considering an empty destination.
func nativeRestoreJob(ctx context.Context, db *sql.DB, planID, subdirectory string, cutoff time.Time) (int64, string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET allow_unsafe_internals=true`); err != nil {
		return 0, "", err
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	collection := "/" + planID + "/database'"
	rows, err := conn.QueryContext(ctx, `SELECT job_id,status,description FROM crdb_internal.jobs WHERE job_type='RESTORE' AND strpos(description,$1)>0`, collection)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var job int64
	var status string
	for rows.Next() {
		var id int64
		var state, description string
		if err := rows.Scan(&id, &state, &description); err != nil {
			return 0, "", err
		}
		if !strings.HasPrefix(description, "RESTORE FROM "+quote(subdirectory)+" IN 'nodelocal://") ||
			!strings.Contains(description, collection+" AS OF SYSTEM TIME "+quote(cutoff.UTC().Format(time.RFC3339Nano))+" ") {
			return 0, "", fmt.Errorf("native restore job %d differs from the selected collection or timestamp; destination is preserved", id)
		}
		if job != 0 {
			return 0, "", fmt.Errorf("multiple native restore jobs use the selected recovery plan; destination is preserved")
		}
		job, status = id, state
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	if job == 0 {
		return 0, "", sql.ErrNoRows
	}
	return job, status, nil
}
