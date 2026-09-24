package delivery

import (
	"context"
)

func (s *persistence) projectByIDInternalQuerier(ctx context.Context, q ServiceQueryer, projectID string) (ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at,
		        deleted_at, deleted_by_user_id, delete_expires_at, log_retention_days
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return scanProjectRow(row)
}

func scanProjectRow(scanner interface{ Scan(...any) error }) (ProjectRecord, error) {
	var rec ProjectRecord
	var kind string
	var tombstone Tombstone
	targets := []any{&rec.ID, &rec.Name, &kind, &rec.SystemKey, &rec.CreatedAt}
	if err := scanner.Scan(append(ScanTombstone(targets, &tombstone), &rec.LogRetentionDays)...); err != nil {
		return ProjectRecord{}, err
	}
	rec.Kind = ProjectKind(kind)
	if rec.Kind == "" {
		rec.Kind = ProjectKindUser
	}
	rec.Deletion = EffectiveDeletion(tombstone)
	return rec, nil
}

func ScanProjectRow(scanner interface{ Scan(...any) error }) (ProjectRecord, error) {
	return scanProjectRow(scanner)
}
