package journal

import (
	"context"
	"database/sql"
	"fmt"
)

// Table names a durable product table recorded by a mutation. Every product
// write inside a journal command must record its durable key so the command
// payload can be resolved without reading unrelated state.
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
	TableDomains        Table = "domain_bindings"
)

// Recorder accumulates the durable keys a journal command touched. Execute
// resolves them into a minimal Batch after the mutation returns, so the payload
// and lock hold time scale with the change, not the cluster.
type Recorder struct {
	tx             *sql.Tx
	keys           map[Table]map[string]struct{}
	domainServices map[string]map[string]struct{}
}

func newRecorder(tx *sql.Tx) *Recorder {
	return &Recorder{tx: tx, keys: make(map[Table]map[string]struct{}), domainServices: make(map[string]map[string]struct{})}
}

type recorderContextKey struct{}

func withRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

// RecorderFromContext returns the recorder of the executing journal command, or
// nil outside one.
func RecorderFromContext(ctx context.Context) *Recorder {
	recorder, _ := ctx.Value(recorderContextKey{}).(*Recorder)
	return recorder
}

// DomainServices maps a changed domain hostname to the services it routes to,
// including a service a domain was reassigned away from.
func (r *Recorder) DomainServices() map[string][]string {
	if r == nil {
		return nil
	}
	out := make(map[string][]string, len(r.domainServices))
	for hostname, services := range r.domainServices {
		out[hostname] = sortedKeys(services)
	}
	return out
}

func (r *Recorder) record(table Table, key string) {
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
	recorder := RecorderFromContext(ctx)
	if recorder == nil {
		panic(fmt.Sprintf("journal: %s row %q changed outside a journal command", table, key))
	}
	recorder.record(table, key)
}

func RecordProject(ctx context.Context, id string)        { record(ctx, TableProjects, id) }
func RecordService(ctx context.Context, id string)        { record(ctx, TableServices, id) }
func RecordAssignment(ctx context.Context, id string)     { record(ctx, TableAssignments, id) }
func RecordDeployment(ctx context.Context, id string)     { record(ctx, TableDeployments, id) }
func RecordAgent(ctx context.Context, id string)          { record(ctx, TableAgents, id) }
func RecordAdministration(ctx context.Context, id string) { record(ctx, TableAdministration, id) }
func RecordEnvironment(ctx context.Context, id string)    { record(ctx, TableEnvironments, id) }
func RecordVolume(ctx context.Context, id string)         { record(ctx, TableVolumes, id) }

// RecordDomain records a domain change and the service it routes to, so the
// affected-agent set can be derived even after the domain row is deleted.
func RecordDomain(ctx context.Context, hostname, serviceID string) {
	record(ctx, TableDomains, hostname)
	recorder := RecorderFromContext(ctx)
	if recorder == nil || serviceID == "" {
		return
	}
	services := recorder.domainServices[hostname]
	if services == nil {
		services = make(map[string]struct{})
		recorder.domainServices[hostname] = services
	}
	services[serviceID] = struct{}{}
}

func RecordRevision(ctx context.Context, serviceID string, revision int64) {
	record(ctx, TableRevisions, compositeKey(serviceID, revision))
}

func RecordRollout(ctx context.Context, serviceID string, generation int64) {
	record(ctx, TableRollouts, compositeKey(serviceID, generation))
}

// RecordServiceRemoval captures the durable rows a service delete cascades to.
// It must run before the DELETE so the command carries the whole cascade.
func RecordServiceRemoval(ctx context.Context, tx *sql.Tx, serviceID string) error {
	RecordService(ctx, serviceID)
	queries := []struct {
		table Table
		sql   string
	}{
		{TableRevisions, `SELECT service_id::STRING || '/' || spec_revision::STRING FROM service_revisions WHERE service_id = $1`},
		{TableRollouts, `SELECT service_id::STRING || '/' || rollout_generation::STRING FROM service_rollouts WHERE service_id = $1`},
		{TableAssignments, `SELECT id::STRING FROM allocation_assignments WHERE service_id = $1`},
		{TableDeployments, `SELECT id::STRING FROM deployments WHERE service_id = $1`},
	}
	for _, query := range queries {
		if err := recordQuery(ctx, tx, query.table, query.sql, serviceID); err != nil {
			return err
		}
	}
	hostnames, err := queryKeys(ctx, tx, `SELECT hostname::STRING FROM domain_bindings WHERE service_id = $1`, serviceID)
	if err != nil {
		return err
	}
	for _, hostname := range hostnames {
		RecordDomain(ctx, hostname, serviceID)
	}
	return nil
}

// RecordEnvironmentRemoval captures every durable row an environment delete
// cascades to. It must run before the DELETE.
func RecordEnvironmentRemoval(ctx context.Context, tx *sql.Tx, environmentID string) error {
	RecordEnvironment(ctx, environmentID)
	serviceIDs, err := queryKeys(ctx, tx, `SELECT id::STRING FROM services WHERE environment_id = $1`, environmentID)
	if err != nil {
		return err
	}
	if err := recordQuery(ctx, tx, TableVolumes, `SELECT id::STRING FROM volumes WHERE environment_id = $1`, environmentID); err != nil {
		return err
	}
	for _, serviceID := range serviceIDs {
		if err := RecordServiceRemoval(ctx, tx, serviceID); err != nil {
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
