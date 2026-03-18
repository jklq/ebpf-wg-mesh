package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) ensureProjectNamed(ctx context.Context, name string) (string, error) {
	return s.ensureProjectNamedQuerier(ctx, s.db, name, projectKindUser, "")
}

func (s *Store) ensureProjectNamedQuerier(ctx context.Context, q serviceQueryer, name string, kind projectKind, systemKey string) (string, error) {
	project, found, err := s.projectByNameQuerier(ctx, q, name)
	if err != nil {
		return "", err
	}
	if found {
		if project.Kind != kind {
			return "", fmt.Errorf("project %q already exists with kind %s", name, project.Kind)
		}
		return project.ID, nil
	}

	id := mustID()
	now := time.Now().UTC()
	if _, err := q.ExecContext(
		ctx,
		`INSERT INTO projects(id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5)`,
		id,
		name,
		string(kind),
		nullIfEmpty(systemKey),
		now,
	); err != nil {
		return "", fmt.Errorf("insert project: %w", err)
	}
	return id, nil
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
				`INSERT INTO projects(id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5)`,
				project.ID,
				project.Name,
				string(project.Kind),
				project.SystemKey,
				project.CreatedAt,
			)
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
		project = current
		return nil
	})
	if err != nil {
		return projectRecord{}, err
	}
	return project, nil
}

func (s *Store) createProject(ctx context.Context, subject, name string) (projectRecord, error) {
	var project projectRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		id, err := s.ensureProjectNamedQuerier(ctx, tx, name, projectKindUser, "")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO project_memberships(subject, project_id, role) VALUES ($1, $2, $3) ON CONFLICT(subject, project_id) DO NOTHING`,
			subject,
			id,
			"owner",
		); err != nil {
			return err
		}
		project, err = s.projectByIDQuerier(ctx, tx, subject, id)
		return err
	})
	if err != nil {
		return projectRecord{}, err
	}
	return project, nil
}

func (s *Store) listProjects(ctx context.Context, subject string) ([]projectRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE m.subject = $1 AND p.kind = $2
		  ORDER BY p.created_at ASC`,
		subject,
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

func (s *Store) projectByID(ctx context.Context, subject, projectID string) (projectRecord, error) {
	return s.projectByIDQuerier(ctx, s.db, subject, projectID)
}

func (s *Store) projectByIDQuerier(ctx context.Context, q serviceQueryer, subject, projectID string) (projectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE p.id = $1 AND m.subject = $2 AND p.kind = $3`,
		projectID,
		subject,
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

func (s *Store) projectByNameQuerier(ctx context.Context, q serviceQueryer, name string) (projectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE name = $1`,
		name,
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

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
