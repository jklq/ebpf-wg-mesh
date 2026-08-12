package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) ensureUserProjectNamed(ctx context.Context, userID, name string) (string, error) {
	return s.ensureUserProjectNamedQuerier(ctx, s.db, userID, name)
}

func (s *Store) ensureUserProjectNamedQuerier(ctx context.Context, q serviceQueryer, userID, name string) (string, error) {
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

	id := mustID()
	now := time.Now().UTC()
	if _, err := q.ExecContext(
		ctx,
		`INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		id,
		userID,
		name,
		string(projectKindUser),
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

func (s *Store) ensureProjectOwnerMembershipQuerier(ctx context.Context, q serviceQueryer, userID, projectID string) error {
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

func (s *Store) ensureManagedProject(ctx context.Context, name, systemKey string) (projectRecord, error) {
	var project projectRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.projectBySystemKeyQuerier(ctx, tx, systemKey)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !found {
			project = projectRecord{
				ID:        mustID(),
				Name:      name,
				Kind:      projectKindManaged,
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
		if current.Name != name || current.Kind != projectKindManaged {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE projects SET name = $1, kind = $2 WHERE id = $3`,
				name,
				string(projectKindManaged),
				current.ID,
			); err != nil {
				return err
			}
			current.Name = name
			current.Kind = projectKindManaged
		}
		if _, err := s.ensureProductionEnvironmentQuerier(ctx, tx, current.ID); err != nil {
			return err
		}
		project = current
		return nil
	})
	if err != nil {
		return projectRecord{}, err
	}
	return project, nil
}

func (s *Store) createProject(ctx context.Context, userID, name string) (projectRecord, error) {
	var project projectRecord
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
		return projectRecord{}, err
	}
	return project, nil
}

func (s *Store) listProjects(ctx context.Context, userID string) ([]projectRecord, error) {
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
		string(projectKindUser),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []projectRecord
	for rows.Next() {
		rec, err := scanProjectRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) projectByID(ctx context.Context, userID, projectID string) (projectRecord, error) {
	return s.projectByIDQuerier(ctx, s.db, userID, projectID)
}

func (s *Store) authorizeProjectWrite(ctx context.Context, userID, projectID string) error {
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
		string(projectKindUser),
	).Scan(&allowed)
}

func (s *Store) projectByIDQuerier(ctx context.Context, q serviceQueryer, userID, projectID string) (projectRecord, error) {
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
		string(projectKindUser),
	)
	return scanProjectRow(row)
}

func (s *Store) projectByIDInternal(ctx context.Context, projectID string) (projectRecord, error) {
	return s.projectByIDInternalQuerier(ctx, s.db, projectID)
}

func (s *Store) projectByIDInternalQuerier(ctx context.Context, q serviceQueryer, projectID string) (projectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return scanProjectRow(row)
}

func (s *Store) projectBySystemKeyQuerier(ctx context.Context, q serviceQueryer, systemKey string) (projectRecord, bool, error) {
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
		return projectRecord{}, false, nil
	default:
		return projectRecord{}, false, err
	}
}

func (s *Store) projectByOwnedNameQuerier(ctx context.Context, q serviceQueryer, userID, name string) (projectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE owner_user_id = $1 AND name = $2 AND kind = $3`,
		userID,
		name,
		string(projectKindUser),
	)
	rec, err := scanProjectRow(row)
	switch {
	case err == nil:
		return rec, true, nil
	case err == sql.ErrNoRows:
		return projectRecord{}, false, nil
	default:
		return projectRecord{}, false, err
	}
}

func scanProjectRow(scanner interface{ Scan(...any) error }) (projectRecord, error) {
	var rec projectRecord
	var kind string
	if err := scanner.Scan(&rec.ID, &rec.Name, &kind, &rec.SystemKey, &rec.CreatedAt); err != nil {
		return projectRecord{}, err
	}
	rec.Kind = projectKind(kind)
	if rec.Kind == "" {
		rec.Kind = projectKindUser
	}
	return rec, nil
}

func allocateEnvironmentNetworkIdentity(ctx context.Context, q serviceQueryer) (uint32, error) {
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
