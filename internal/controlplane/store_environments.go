package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"

	"github.com/google/uuid"
)

var errProductionEnvironment = errors.New("production environment cannot be deleted")

func (s *catalogPersistence) ensureProductionEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) (deliverycore.EnvironmentRecord, error) {
	rec, err := deliverycore.ScanEnvironmentRow(q.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return deliverycore.EnvironmentRecord{}, err
	}
	return s.createEnvironmentQuerier(ctx, q, projectID, "Production", true, "")
}

func (s *catalogPersistence) createEnvironment(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = s.createEnvironmentQuerier(ctx, tx, scope.ID(), name, false, "")
		return err
	})
	return rec, err
}

func (s *catalogPersistence) listEnvironments(ctx context.Context, user authz.User, projectID string) ([]deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1
		 ORDER BY e.is_production DESC, e.created_at ASC, e.id ASC`, scope.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deliverycore.EnvironmentRecord
	for rows.Next() {
		rec, err := deliverycore.ScanEnvironmentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *catalogPersistence) productionEnvironmentByProjectInternal(ctx context.Context, projectID string) (deliverycore.EnvironmentRecord, error) {
	return deliverycore.ScanEnvironmentRow(s.db.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
}

func (s *catalogPersistence) renameEnvironment(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deliverycore.EnvironmentRecord{}, fmt.Errorf("environment name is required")
	}
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.environmentByScopeQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE environments SET name = $1, updated_at = $2 WHERE id = $3`, name, now, current.ID); err != nil {
			return err
		}
		journal.RecordEnvironment(ctx, current.ID)
		current.Name = name
		current.UpdatedAt = now
		rec = current
		return nil
	})
	return rec, err
}

func (s *catalogPersistence) deleteEnvironment(ctx context.Context, user authz.User, environmentID string) ([]string, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return nil, err
	}
	var agentIDs []string
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		agentIDs = nil
		rec, err := s.environmentByScopeQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if rec.IsProduction {
			return errProductionEnvironment
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.agent_id FROM allocations a
			JOIN services s ON s.id = a.service_id WHERE s.environment_id = $1`, rec.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			agentIDs = append(agentIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := journal.RecordEnvironmentRemoval(ctx, tx, rec.ID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM environments WHERE id = $1`, rec.ID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		if len(agentIDs) == 0 {
			return nil
		}
		agentIDs, err = s.agentIDsQuerier(ctx, tx)
		return err
	})
	return agentIDs, err
}

func (s *catalogPersistence) createEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID, name string, production bool, copiedFrom string) (deliverycore.EnvironmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deliverycore.EnvironmentRecord{}, fmt.Errorf("environment name is required")
	}
	networkIdentity, err := allocateEnvironmentNetworkIdentity(ctx, q)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	now := time.Now().UTC()
	rec := deliverycore.EnvironmentRecord{
		ID:                      uuid.NewString(),
		ProjectID:               projectID,
		Name:                    name,
		Kind:                    deliverycore.EnvironmentKindPersistent,
		IsProduction:            production,
		NetworkIdentity:         networkIdentity,
		CopiedFromEnvironmentID: copiedFrom,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO environments(
			id, project_id, name, kind, is_production, network_identity,
			copied_from_environment_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		rec.ID, rec.ProjectID, rec.Name, string(rec.Kind), rec.IsProduction,
		rec.NetworkIdentity, nullIfEmpty(rec.CopiedFromEnvironmentID), rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	journal.RecordEnvironment(ctx, rec.ID)
	return rec, nil
}

func allocateEnvironmentNetworkIdentity(ctx context.Context, q deliverycore.ServiceQueryer) (uint32, error) {
	var identity int64
	if err := q.QueryRowContext(ctx,
		`UPDATE environment_network_identity_counter
		    SET next_identity = next_identity + 1
		  WHERE id = TRUE AND next_identity <= 4294967295
		  RETURNING next_identity - 1`,
	).Scan(&identity); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("environment network identity space exhausted")
		}
		return 0, fmt.Errorf("allocate environment network identity: %w", err)
	}
	return uint32(identity), nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *catalogPersistence) environmentByScopeQuerier(ctx context.Context, q deliverycore.ServiceQueryer, scope authz.Environment) (deliverycore.EnvironmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2`,
		scope.ID(), scope.ProjectID())
	return deliverycore.ScanEnvironmentRow(row)
}

const environmentSelect = `SELECT e.id, e.project_id, e.name, e.kind, e.is_production,
	e.network_identity, COALESCE(e.copied_from_environment_id, ''), e.created_at, e.updated_at
	FROM environments e`

func (s *catalogPersistence) agentIDsQuerier(ctx context.Context, q deliverycore.ServiceQueryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM agents ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
