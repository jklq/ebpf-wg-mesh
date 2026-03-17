package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) ensureProjectNamed(ctx context.Context, name string) (string, error) {
	return s.ensureProjectNamedQuerier(ctx, s.db, name)
}

func (s *Store) ensureProjectNamedQuerier(ctx context.Context, q serviceQueryer, name string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = $1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = mustID()
	now := time.Now().UTC()
	if _, err := q.ExecContext(ctx, `INSERT INTO projects(id, name, created_at) VALUES ($1, $2, $3)`, id, name, now); err != nil {
		return "", fmt.Errorf("insert project: %w", err)
	}
	return id, nil
}

func (s *Store) createProject(ctx context.Context, subject, name string) (projectRecord, error) {
	var project projectRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		id, err := s.ensureProjectNamedQuerier(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_memberships(subject, project_id, role) VALUES ($1, $2, $3) ON CONFLICT(subject, project_id) DO NOTHING`,
			subject, id, "owner",
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE m.subject = $1
		  ORDER BY p.created_at ASC`,
		subject,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []projectRecord
	for rows.Next() {
		var rec projectRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.CreatedAt); err != nil {
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
	var rec projectRecord
	err := q.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.created_at
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE p.id = $1 AND m.subject = $2`,
		projectID, subject,
	).Scan(&rec.ID, &rec.Name, &rec.CreatedAt)
	if err != nil {
		return projectRecord{}, err
	}
	return rec, nil
}
