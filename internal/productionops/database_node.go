package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
)

// Resolve actual gossip identities independently of SQL gateway ports or
// placement ordinals. Bracketed IPv6 and custom native ports are supported.
func databaseNodeID(ctx context.Context, db *sql.DB, address string) (int64, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET allow_unsafe_internals = true`); err != nil {
		return 0, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT node_id,address FROM crdb_internal.gossip_nodes`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var selected int64
	for rows.Next() {
		var id int64
		var actual string
		if err := rows.Scan(&id, &actual); err != nil {
			return 0, err
		}
		host, _, err := net.SplitHostPort(actual)
		if err != nil {
			return 0, err
		}
		if !sameDatabaseHost(address, host) {
			continue
		}
		if selected != 0 {
			return 0, fmt.Errorf("database host has ambiguous native node identities")
		}
		selected = id
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if selected == 0 {
		return 0, sql.ErrNoRows
	}
	return selected, nil
}

func sameDatabaseHost(expected, observed string) bool {
	a, b := net.ParseIP(strings.Trim(expected, "[]")), net.ParseIP(strings.Trim(observed, "[]"))
	if a != nil || b != nil {
		return a != nil && b != nil && a.Equal(b)
	}
	return strings.EqualFold(strings.TrimSuffix(expected, "."), strings.TrimSuffix(observed, "."))
}
