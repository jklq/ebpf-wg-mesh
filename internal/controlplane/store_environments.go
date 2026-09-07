package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errProductionEnvironment = errors.New("production environment cannot be deleted")

func (s *Store) ensureProductionEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) (deliverycore.EnvironmentRecord, error) {
	rec, err := deliverycore.ScanEnvironmentRow(q.QueryRowContext(ctx, deliverycore.EnvironmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return deliverycore.EnvironmentRecord{}, err
	}
	return s.createEnvironmentQuerier(ctx, q, projectID, "Production", true, "")
}

func (s *Store) createEnvironment(ctx context.Context, userID, projectID, name string) (deliverycore.EnvironmentRecord, error) {
	var rec deliverycore.EnvironmentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.authorizeProjectWriteQuerier(ctx, tx, userID, projectID); err != nil {
			return err
		}
		var err error
		rec, err = s.createEnvironmentQuerier(ctx, tx, projectID, name, false, "")
		return err
	})
	return rec, err
}

func (s *Store) listEnvironments(ctx context.Context, userID, projectID string) ([]deliverycore.EnvironmentRecord, error) {
	if _, err := s.projectByID(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, deliverycore.EnvironmentSelect+`
		 WHERE e.project_id = $1
		 ORDER BY e.is_production DESC, e.created_at ASC, e.id ASC`, projectID)
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

func (s *Store) environmentByIDInternalQuerier(ctx context.Context, q deliverycore.ServiceQueryer, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return deliverycore.ScanEnvironmentRow(q.QueryRowContext(ctx, deliverycore.EnvironmentSelect+` WHERE e.id = $1`, environmentID))
}

func (s *Store) productionEnvironmentByProjectInternal(ctx context.Context, projectID string) (deliverycore.EnvironmentRecord, error) {
	return deliverycore.ScanEnvironmentRow(s.db.QueryRowContext(ctx, deliverycore.EnvironmentSelect+`
		 WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
}

func (s *Store) authorizeEnvironmentWrite(ctx context.Context, userID, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.deliveryQueries().AuthorizeEnvironmentWriteQuerier(ctx, s.db, userID, environmentID)
}

func (s *Store) authorizeProjectWriteQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, projectID string) error {
	var allowed bool
	return q.QueryRowContext(ctx, `SELECT TRUE FROM projects p
		JOIN project_memberships m ON m.project_id = p.id
		WHERE p.id = $1 AND m.user_id = $2
		  AND m.role IN ('owner', 'editor') AND p.kind = $3`,
		projectID, userID, string(deliverycore.ProjectKindUser)).Scan(&allowed)
}

func (s *Store) renameEnvironment(ctx context.Context, userID, environmentID, name string) (deliverycore.EnvironmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deliverycore.EnvironmentRecord{}, fmt.Errorf("environment name is required")
	}
	var rec deliverycore.EnvironmentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := s.deliveryQueries().AuthorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE environments SET name = $1, updated_at = $2 WHERE id = $3`, name, now, environmentID); err != nil {
			return err
		}
		current.Name = name
		current.UpdatedAt = now
		rec = current
		return nil
	})
	return rec, err
}

func (s *Store) deleteEnvironment(ctx context.Context, userID, environmentID string) ([]string, error) {
	var agentIDs []string
	var identityCatalogChanged bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agentIDs = nil
		identityCatalogChanged = false
		rec, err := s.deliveryQueries().AuthorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
		if err != nil {
			return err
		}
		if rec.IsProduction {
			return errProductionEnvironment
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.agent_id FROM allocations a
			JOIN services s ON s.id = a.service_id WHERE s.environment_id = $1`, environmentID)
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
		result, err := tx.ExecContext(ctx, `DELETE FROM environments WHERE id = $1`, environmentID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		identityCatalogChanged = len(agentIDs) > 0
		if !identityCatalogChanged {
			return nil
		}
		if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
			return err
		}
		agentIDs, err = s.deliveryQueries().AgentIDsQuerier(ctx, tx)
		return err
	})
	return agentIDs, err
}

func (s *Store) createEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID, name string, production bool, copiedFrom string) (deliverycore.EnvironmentRecord, error) {
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
		ID:                      deliverycore.MustID(),
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
