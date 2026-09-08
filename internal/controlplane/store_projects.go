package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *catalogPersistence) ensureUserProjectNamed(ctx context.Context, userID, name string) (string, error) {
	return s.ensureUserProjectNamedQuerier(ctx, s.db, userID, name)
}

func (s *catalogPersistence) ensureUserProjectNamedQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, name string) (string, error) {
	project, found, err := s.projectByOwnedNameQuerier(ctx, q, userID, name)
	if err != nil {
		return "", err
	}
	if found {
		if _, err := s.ensureProductionEnvironmentQuerier(ctx, q, project.ID); err != nil {
			return "", fmt.Errorf("ensure production environment: %w", err)
		}
		return project.ID, nil
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := q.ExecContext(
		ctx,
		`INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		id,
		userID,
		name,
		string(deliverycore.ProjectKindUser),
		nil,
		now,
	); err != nil {
		return "", fmt.Errorf("insert project: %w", err)
	}
	if _, err := s.createEnvironmentQuerier(ctx, q, id, "Production", true, ""); err != nil {
		return "", fmt.Errorf("create production environment: %w", err)
	}
	return id, nil
}

func (s *catalogPersistence) ensureProjectOwnerMembershipQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, projectID string) error {
	_, err := q.ExecContext(
		ctx,
		`INSERT INTO project_memberships(user_id, project_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT(user_id, project_id) DO UPDATE SET role = excluded.role`,
		userID,
		projectID,
		"owner",
	)
	return err
}

func (s *catalogPersistence) ensureManagedProject(ctx context.Context, name, systemKey string) (deliverycore.ProjectRecord, error) {
	var project deliverycore.ProjectRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.projectBySystemKeyQuerier(ctx, tx, systemKey)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !found {
			project = deliverycore.ProjectRecord{
				ID:        uuid.NewString(),
				Name:      name,
				Kind:      deliverycore.ProjectKindManaged,
				SystemKey: systemKey,
				CreatedAt: now,
			}
			_, err = tx.ExecContext(
				ctx,
				`INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				project.ID,
				"",
				project.Name,
				string(project.Kind),
				project.SystemKey,
				project.CreatedAt,
			)
			if err != nil {
				return err
			}
			_, err = s.createEnvironmentQuerier(ctx, tx, project.ID, "Production", true, "")
			return err
		}
		if current.Name != name || current.Kind != deliverycore.ProjectKindManaged {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE projects SET name = $1, kind = $2 WHERE id = $3`,
				name,
				string(deliverycore.ProjectKindManaged),
				current.ID,
			); err != nil {
				return err
			}
			current.Name = name
			current.Kind = deliverycore.ProjectKindManaged
		}
		if _, err := s.ensureProductionEnvironmentQuerier(ctx, tx, current.ID); err != nil {
			return err
		}
		project = current
		return nil
	})
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return project, nil
}

func (s *catalogPersistence) createProject(ctx context.Context, userID, name string) (deliverycore.ProjectRecord, error) {
	var project deliverycore.ProjectRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		id, err := s.ensureUserProjectNamedQuerier(ctx, tx, userID, name)
		if err != nil {
			return err
		}
		if err := s.ensureProjectOwnerMembershipQuerier(ctx, tx, userID, id); err != nil {
			return err
		}
		project, err = s.projectByIDQuerier(ctx, tx, userID, id)
		return err
	})
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return project, nil
}

func (s *catalogPersistence) listProjects(ctx context.Context, userID string) ([]deliverycore.ProjectRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE m.user_id = $1
		    AND m.role IN ('owner', 'editor', 'viewer')
		    AND p.kind = $2
		  ORDER BY p.created_at ASC`,
		userID,
		string(deliverycore.ProjectKindUser),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deliverycore.ProjectRecord
	for rows.Next() {
		rec, err := scanProjectRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *catalogPersistence) projectByID(ctx context.Context, userID, projectID string) (deliverycore.ProjectRecord, error) {
	return s.projectByIDQuerier(ctx, s.db, userID, projectID)
}

func (s *catalogPersistence) authorizeProjectWrite(ctx context.Context, userID, projectID string) error {
	var allowed bool
	return s.db.QueryRowContext(
		ctx,
		`SELECT TRUE
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE p.id = $1
		    AND m.user_id = $2
		    AND m.role IN ('owner', 'editor')
		    AND p.kind = $3`,
		projectID,
		userID,
		string(deliverycore.ProjectKindUser),
	).Scan(&allowed)
}

func (s *catalogPersistence) projectByIDQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, projectID string) (deliverycore.ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE p.id = $1
		    AND m.user_id = $2
		    AND m.role IN ('owner', 'editor', 'viewer')
		    AND p.kind = $3`,
		projectID,
		userID,
		string(deliverycore.ProjectKindUser),
	)
	return scanProjectRow(row)
}

func (s *catalogPersistence) projectByIDInternal(ctx context.Context, projectID string) (deliverycore.ProjectRecord, error) {
	return s.projectByIDInternalQuerier(ctx, s.db, projectID)
}

func (s *catalogPersistence) projectBySystemKeyQuerier(ctx context.Context, q deliverycore.ServiceQueryer, systemKey string) (deliverycore.ProjectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE system_key = $1`,
		systemKey,
	)
	rec, err := scanProjectRow(row)
	switch {
	case err == nil:
		return rec, true, nil
	case err == sql.ErrNoRows:
		return deliverycore.ProjectRecord{}, false, nil
	default:
		return deliverycore.ProjectRecord{}, false, err
	}
}

func (s *catalogPersistence) projectByOwnedNameQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, name string) (deliverycore.ProjectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE owner_user_id = $1 AND name = $2 AND kind = $3`,
		userID,
		name,
		string(deliverycore.ProjectKindUser),
	)
	rec, err := scanProjectRow(row)
	switch {
	case err == nil:
		return rec, true, nil
	case err == sql.ErrNoRows:
		return deliverycore.ProjectRecord{}, false, nil
	default:
		return deliverycore.ProjectRecord{}, false, err
	}
}

func (s *catalogPersistence) projectByIDInternalQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) (deliverycore.ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return scanProjectRow(row)
}

func scanProjectRow(scanner interface{ Scan(...any) error }) (deliverycore.ProjectRecord, error) {
	var rec deliverycore.ProjectRecord
	var kind string
	if err := scanner.Scan(&rec.ID, &rec.Name, &kind, &rec.SystemKey, &rec.CreatedAt); err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	rec.Kind = deliverycore.ProjectKind(kind)
	if rec.Kind == "" {
		rec.Kind = deliverycore.ProjectKindUser
	}
	return rec, nil
}
