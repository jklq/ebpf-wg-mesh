package journal

import (
	"context"
	"database/sql"
)

// Mutation couples a persistence operation to its product effects. The SQL
// remains owned by the domain operation; the journal never parses SQL or guesses
// which read-model rows a write affects (for example, delivery status is part of
// a ServiceRow). Capture and SQL execution use the same command transaction.
type Mutation struct{ rows []productRow }
type productRow struct {
	table   Table
	key     string
	subtree bool
}

func row(table Table, key string) Mutation {
	return Mutation{rows: []productRow{{table: table, key: key}}}
}
func ProjectRow(id string) Mutation        { return row(TableProjects, id) }
func ServiceRow(id string) Mutation        { return row(TableServices, id) }
func AssignmentRow(id string) Mutation     { return row(TableAssignments, id) }
func DeploymentRow(id string) Mutation     { return row(TableDeployments, id) }
func AgentRow(id string) Mutation          { return row(TableAgents, id) }
func AdministrationRow(id string) Mutation { return row(TableAdministration, id) }
func EnvironmentRow(id string) Mutation    { return row(TableEnvironments, id) }
func VolumeRow(id string) Mutation         { return row(TableVolumes, id) }
func DomainRow(hostname string) Mutation   { return row(TableDomains, hostname) }
func RevisionRow(serviceID string, revision int64) Mutation {
	return row(TableRevisions, compositeKey(serviceID, revision))
}
func RolloutRow(serviceID string, generation int64) Mutation {
	return row(TableRollouts, compositeKey(serviceID, generation))
}

// Subtree mutations cover visibility changes caused by tombstoning, restoring,
// or deleting a parent. Descendant keys are captured before the SQL changes them.
func ServiceTree(id string) Mutation     { return subtree(TableServices, id) }
func EnvironmentTree(id string) Mutation { return subtree(TableEnvironments, id) }
func ProjectTree(id string) Mutation     { return subtree(TableProjects, id) }
func subtree(table Table, key string) Mutation {
	return Mutation{rows: []productRow{{table: table, key: key, subtree: true}}}
}

func Rows(effects ...Mutation) Mutation {
	var rows []productRow
	for _, effect := range effects {
		rows = append(rows, effect.rows...)
	}
	return Mutation{rows: rows}
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m Mutation) Exec(ctx context.Context, q execer, query string, args ...any) (sql.Result, error) {
	if err := m.Capture(ctx); err != nil {
		return nil, err
	}
	return q.ExecContext(ctx, query, args...)
}

type MutationRow struct {
	row *sql.Row
	err error
}

func (m Mutation) QueryRow(ctx context.Context, q rowQueryer, query string, args ...any) MutationRow {
	if err := m.Capture(ctx); err != nil {
		return MutationRow{err: err}
	}
	return MutationRow{row: q.QueryRowContext(ctx, query, args...)}
}
func (r MutationRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

// Capture is for operations that execute writes through another persistence
// interface (and test fixtures); direct SQL writes use Exec or QueryRow so their
// effects cannot be omitted separately from the write.
func (m Mutation) Capture(ctx context.Context) error {
	r := recorderFromContext(ctx)
	if r == nil {
		panic("journal: product mutation outside a journal command")
	}
	for _, row := range m.rows {
		if !row.subtree {
			r.record(row.table, row.key)
			continue
		}
		var err error
		switch row.table {
		case TableServices:
			err = recordServiceTree(ctx, r.tx, row.key)
		case TableEnvironments:
			err = recordEnvironmentTree(ctx, r.tx, row.key)
		case TableProjects:
			err = recordProjectTree(ctx, r.tx, row.key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// UpdateRows captures the explicit primary keys returned by a bulk mutation.
// The SQL must return one product key column; auxiliary writes use their owning
// product table here, just as an ordinary row mutation does.
func UpdateRows(ctx context.Context, tx *sql.Tx, table Table, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		record(ctx, table, id)
	}
	return rows.Err()
}
