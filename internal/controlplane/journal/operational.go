package journal

import (
	"context"
	"database/sql"
	"strings"
)

// A reference change can make an unchanged historical row operational again.
// Resolve the closure for affected services, including old resident keys so
// rows leaving that closure produce journal deletions. SQL history is untouched.
func recordOperationalDependencies(ctx context.Context, tx *sql.Tx, base DurableState, recorded map[Table]map[string]struct{}) error {
	services := make(map[string]struct{})
	add := func(id string) {
		if id != "" {
			services[id] = struct{}{}
		}
	}
	for id := range recorded[TableServices] {
		add(id)
	}
	for _, table := range []Table{TableRevisions, TableRollouts} {
		for key := range recorded[table] {
			id, _, _ := strings.Cut(key, "/")
			add(id)
		}
	}
	for _, table := range []Table{TableAssignments, TableDeployments} {
		keys := sortedKeys(recorded[table])
		if len(keys) == 0 {
			continue
		}
		for _, key := range keys {
			if table == TableAssignments {
				add(base.Assignments[key].ServiceID)
			} else {
				add(base.Deployments[key].ServiceID)
			}
		}
		where, args := singleKeyWhere("id", keys)
		ids, err := queryKeys(ctx, tx, `SELECT DISTINCT service_id::TEXT FROM `+string(table)+` WHERE `+where, args...)
		if err != nil {
			return err
		}
		for _, id := range ids {
			add(id)
		}
	}
	if len(services) == 0 {
		return nil
	}
	remember := func(table Table, key, serviceID string) {
		if _, ok := services[serviceID]; !ok {
			return
		}
		if recorded[table] == nil {
			recorded[table] = make(map[string]struct{})
		}
		recorded[table][key] = struct{}{}
	}
	for key, value := range base.Revisions {
		remember(TableRevisions, key, value.ServiceID)
	}
	for key, value := range base.Rollouts {
		remember(TableRollouts, key, value.ServiceID)
	}
	for key, value := range base.Deployments {
		remember(TableDeployments, key, value.ServiceID)
	}
	where, args := singleKeyWhere("service_id", sortedKeys(services))
	for _, table := range productTables {
		if table.id() != TableRevisions && table.id() != TableRollouts && table.id() != TableDeployments {
			continue
		}
		keys, err := queryKeys(ctx, tx, `SELECT `+table.keySQL+` FROM `+table.name+` WHERE (`+where+`) AND (`+table.filter+`)`, args...)
		if err != nil {
			return err
		}
		if recorded[table.id()] == nil {
			recorded[table.id()] = make(map[string]struct{})
		}
		for _, key := range keys {
			recorded[table.id()][key] = struct{}{}
		}
	}
	return nil
}
