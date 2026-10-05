// Package ingressnodes owns durable ingress membership and xDS observations.
// Connectivity never changes operator intent. Retirement is permanent: delayed
// observations, process restarts, and certificate renewal cannot activate a tombstone.
package ingressnodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

var ErrRetired = errors.New("ingress node is permanently retired")

type Registry struct{ db *sql.DB }

func New(db *sql.DB) *Registry { return &Registry{db: db} }

// Register establishes the safety barrier before credentials leave provisioning.
// Repeating registration for an active ID is safe and does not erase its ACKs.
func (r *Registry) Register(ctx context.Context, id string) error {
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("stable ingress node ID is required without surrounding whitespace")
	}
	var state string
	err := r.db.QueryRowContext(ctx, `INSERT INTO ingress_nodes(node_id, state, created_at)
		VALUES ($1, 'active', statement_timestamp())
		ON CONFLICT (node_id) DO UPDATE SET node_id = EXCLUDED.node_id RETURNING state`, id).Scan(&state)
	if err != nil {
		return err
	}
	if state != "active" {
		return ErrRetired
	}
	return nil
}

func (r *Registry) NodeActive(ctx context.Context, id string) (bool, error) {
	var active bool
	err := r.db.QueryRowContext(ctx, `SELECT state = 'active' FROM ingress_nodes WHERE node_id = $1`, id).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return active, err
}

// Retire is an operator assertion that this instance has been removed from
// traffic and stopped. The retained row fences credentials and stale reports.
func (r *Registry) Retire(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE ingress_nodes SET state = 'retired',
		retired_at = COALESCE(retired_at, statement_timestamp()) WHERE node_id = $1`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("unknown ingress node %q", id)
	}
	return nil
}

type Node struct {
	ID, State string
	CreatedAt time.Time
	RetiredAt sql.NullTime
}

func (r *Registry) List(ctx context.Context) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT node_id, state, created_at, retired_at FROM ingress_nodes ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.State, &node.CreatedAt, &node.RetiredAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (r *Registry) UpsertNodeObservations(ctx context.Context, observations []xds.NodeObservation) error {
	for _, observation := range observations {
		// Reports cannot create membership. This statement races retirement safely
		// under serializable SQL; even a retained observation never joins the barrier.
		if _, err := r.db.ExecContext(ctx, `INSERT INTO xds_node_observations(node_id, applied_version, nacks, last_nack, updated_at)
			SELECT node_id, $2, $3, $4, statement_timestamp() FROM ingress_nodes WHERE node_id = $1 AND state = 'active'
			ON CONFLICT (node_id) DO UPDATE SET
				applied_version = CASE WHEN EXCLUDED.applied_version = '' THEN xds_node_observations.applied_version ELSE EXCLUDED.applied_version END,
				nacks = GREATEST(xds_node_observations.nacks, EXCLUDED.nacks),
				last_nack = CASE WHEN EXCLUDED.last_nack = '' THEN xds_node_observations.last_nack ELSE EXCLUDED.last_nack END,
				updated_at = EXCLUDED.updated_at`, observation.NodeID, observation.AppliedVersion, observation.NACKs, observation.LastNACK); err != nil {
			return err
		}
	}
	return nil
}

// ListNodeObservations returns all active members, including provisioned nodes
// that have never connected. There is deliberately no timeout or last-seen filter.
func (r *Registry) ListNodeObservations(ctx context.Context) ([]xds.NodeObservation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT n.node_id, COALESCE(o.applied_version, ''), COALESCE(o.nacks, 0), COALESCE(o.last_nack, '')
		FROM ingress_nodes n LEFT JOIN xds_node_observations o ON o.node_id = n.node_id WHERE n.state = 'active' ORDER BY n.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []xds.NodeObservation
	for rows.Next() {
		var observation xds.NodeObservation
		if err := rows.Scan(&observation.NodeID, &observation.AppliedVersion, &observation.NACKs, &observation.LastNACK); err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, rows.Err()
}
