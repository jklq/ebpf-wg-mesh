package delivery

import (
	"context"
)

func (s *persistence) projectByIDInternalQuerier(ctx context.Context, q ServiceQueryer, projectID string) (ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return scanProjectRow(row)
}

func scanProjectRow(scanner interface{ Scan(...any) error }) (ProjectRecord, error) {
	var rec ProjectRecord
	var kind string
	if err := scanner.Scan(&rec.ID, &rec.Name, &kind, &rec.SystemKey, &rec.CreatedAt); err != nil {
		return ProjectRecord{}, err
	}
	rec.Kind = ProjectKind(kind)
	if rec.Kind == "" {
		rec.Kind = ProjectKindUser
	}
	return rec, nil
}

func ScanProjectRow(scanner interface{ Scan(...any) error }) (ProjectRecord, error) {
	return scanProjectRow(scanner)
}
