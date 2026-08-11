package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) ensureUserProjectNamed(ctx context.Context, subject, name string) (string, error) {
	return s.ensureUserProjectNamedQuerier(ctx, s.db, subject, name)
}

func (s *Store) ensureUserProjectNamedQuerier(ctx context.Context, q serviceQueryer, subject, name string) (string, error) {
	project, found, err := s.projectByOwnedNameQuerier(ctx, q, subject, name)
	if err != nil {
		return "", err
	}
	if found {
		return project.ID, nil
	}

	id := mustID()
	now := time.Now().UTC()
	networkIdentity, err := allocateProjectNetworkIdentity(ctx, q)
	if err != nil {
		return "", err
	}
	if _, err := q.ExecContext(
		ctx,
		`INSERT INTO projects(id, owner_subject, name, kind, system_key, network_identity, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id,
		subject,
		name,
		string(projectKindUser),
		nil,
		networkIdentity,
		now,
	); err != nil {
		return "", fmt.Errorf("insert project: %w", err)
	}
	return id, nil
}

func (s *Store) ensureProjectOwnerMembershipQuerier(ctx context.Context, q serviceQueryer, subject, projectID string) error {
	_, err := q.ExecContext(
		ctx,
		`INSERT INTO project_memberships(subject, project_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT(subject, project_id) DO UPDATE SET role = excluded.role`,
		subject,
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
			networkIdentity, err := allocateProjectNetworkIdentity(ctx, tx)
			if err != nil {
				return err
			}
			project = projectRecord{
				ID:              mustID(),
				Name:            name,
				Kind:            projectKindManaged,
				SystemKey:       systemKey,
				NetworkIdentity: networkIdentity,
				CreatedAt:       now,
			}
			_, err = tx.ExecContext(
				ctx,
				`INSERT INTO projects(id, name, kind, system_key, network_identity, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				project.ID,
				project.Name,
				string(project.Kind),
				project.SystemKey,
				project.NetworkIdentity,
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
		id, err := s.ensureUserProjectNamedQuerier(ctx, tx, subject, name)
		if err != nil {
			return err
		}
		if err := s.ensureProjectOwnerMembershipQuerier(ctx, tx, subject, id); err != nil {
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
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.network_identity, p.created_at
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
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.network_identity, p.created_at
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
		`SELECT id, name, kind, COALESCE(system_key, ''), network_identity, created_at
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return scanProjectRow(row)
}

func (s *Store) projectBySystemKeyQuerier(ctx context.Context, q serviceQueryer, systemKey string) (projectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), network_identity, created_at
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

func (s *Store) projectByOwnedNameQuerier(ctx context.Context, q serviceQueryer, subject, name string) (projectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), network_identity, created_at
		   FROM projects
		  WHERE owner_subject = $1 AND name = $2 AND kind = $3`,
		subject,
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
	var networkIdentity int64
	if err := scanner.Scan(&rec.ID, &rec.Name, &kind, &rec.SystemKey, &networkIdentity, &rec.CreatedAt); err != nil {
		return projectRecord{}, err
	}
	if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
		return projectRecord{}, fmt.Errorf("project %s has invalid network identity %d", rec.ID, networkIdentity)
	}
	rec.NetworkIdentity = uint32(networkIdentity)
	rec.Kind = projectKind(kind)
	if rec.Kind == "" {
		rec.Kind = projectKindUser
	}
	return rec, nil
}

func allocateProjectNetworkIdentity(ctx context.Context, q serviceQueryer) (uint32, error) {
	var identity int64
	if err := q.QueryRowContext(ctx,
		`UPDATE project_network_identity_counter
		    SET next_identity = next_identity + 1
		  WHERE id = TRUE AND next_identity <= 4294967295
		  RETURNING next_identity - 1`,
	).Scan(&identity); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("project network identity space exhausted")
		}
		return 0, fmt.Errorf("allocate project network identity: %w", err)
	}
	return uint32(identity), nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
