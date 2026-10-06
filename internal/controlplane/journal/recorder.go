package journal

import (
	"context"
	"database/sql"
	"fmt"
)

type Table string

const (
	TableProjects       Table = "projects"
	TableServices       Table = "services"
	TableRevisions      Table = "service_revisions"
	TableAssignments    Table = "allocation_assignments"
	TableRollouts       Table = "service_rollouts"
	TableDeployments    Table = "deployments"
	TableAgents         Table = "agent_registrations"
	TableAdministration Table = "agent_administration"
	TableEnvironments   Table = "environments"
	TableVolumes        Table = "volumes"
	TableDestructions   Table = "volume_destructions"
	TableDomains        Table = "domain_bindings"
)

type recorder struct {
	tx   *sql.Tx
	keys map[Table]map[string]struct{}
}

func newRecorder(tx *sql.Tx) *recorder {
	return &recorder{tx: tx, keys: make(map[Table]map[string]struct{})}
}

type recorderContextKey struct{}

func withRecorder(ctx context.Context, recorder *recorder) context.Context {
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

func recorderFromContext(ctx context.Context) *recorder {
	recorder, _ := ctx.Value(recorderContextKey{}).(*recorder)
	return recorder
}

func (r *recorder) record(table Table, key string) {
	if key == "" {
		panic(fmt.Sprintf("journal: empty durable key recorded for %s", table))
	}
	keys := r.keys[table]
	if keys == nil {
		keys = make(map[string]struct{})
		r.keys[table] = keys
	}
	keys[key] = struct{}{}
}

func record(ctx context.Context, table Table, key string) {
	recorder := recorderFromContext(ctx)
	if recorder == nil {
		panic(fmt.Sprintf("journal: %s row %q changed outside a journal command", table, key))
	}
	recorder.record(table, key)
}

func recordServiceTree(ctx context.Context, tx *sql.Tx, serviceID string) error {
	record(ctx, TableServices, serviceID)
	queries := []struct {
		table Table
		sql   string
	}{
		{TableRevisions, `SELECT service_id::TEXT || '/' || spec_revision::TEXT FROM service_revisions WHERE service_id = $1`},
		{TableRollouts, `SELECT service_id::TEXT || '/' || rollout_generation::TEXT FROM service_rollouts WHERE service_id = $1`},
		{TableAssignments, `SELECT id::TEXT FROM allocation_assignments WHERE service_id = $1`},
		{TableDeployments, `SELECT id::TEXT FROM deployments WHERE service_id = $1`},
	}
	for _, query := range queries {
		if err := recordQuery(ctx, tx, query.table, query.sql, serviceID); err != nil {
			return err
		}
	}
	hostnames, err := queryKeys(ctx, tx, `SELECT hostname::TEXT FROM domain_bindings WHERE service_id = $1`, serviceID)
	if err != nil {
		return err
	}
	for _, hostname := range hostnames {
		record(ctx, TableDomains, hostname)
	}
	return nil
}

func recordEnvironmentTree(ctx context.Context, tx *sql.Tx, environmentID string) error {
	record(ctx, TableEnvironments, environmentID)
	serviceIDs, err := queryKeys(ctx, tx, `SELECT id::TEXT FROM services WHERE environment_id = $1`, environmentID)
	if err != nil {
		return err
	}
	if err := recordQuery(ctx, tx, TableVolumes, `SELECT id::TEXT FROM volumes WHERE environment_id = $1`, environmentID); err != nil {
		return err
	}
	for _, serviceID := range serviceIDs {
		if err := recordServiceTree(ctx, tx, serviceID); err != nil {
			return err
		}
	}
	return nil
}

func recordProjectTree(ctx context.Context, tx *sql.Tx, projectID string) error {
	record(ctx, TableProjects, projectID)
	environmentIDs, err := queryKeys(ctx, tx, `SELECT id::TEXT FROM environments WHERE project_id = $1`, projectID)
	if err != nil {
		return err
	}
	for _, environmentID := range environmentIDs {
		if err := recordEnvironmentTree(ctx, tx, environmentID); err != nil {
			return err
		}
	}
	return nil
}

func recordQuery(ctx context.Context, tx *sql.Tx, table Table, query string, args ...any) error {
	keys, err := queryKeys(ctx, tx, query, args...)
	if err != nil {
		return err
	}
	for _, key := range keys {
		record(ctx, table, key)
	}
	return nil
}

func queryKeys(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
