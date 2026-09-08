package delivery

import (
	"context"
	"database/sql"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const environmentSelect = `SELECT e.id, e.project_id, e.name, e.kind, e.is_production,
	e.network_identity, COALESCE(e.copied_from_environment_id, ''), e.created_at, e.updated_at
	FROM environments e`

func (s *persistence) environmentByID(ctx context.Context, userID, environmentID string) (EnvironmentRecord, error) {
	return s.environmentByIDQuerier(ctx, s.db, userID, environmentID)
}

func (s *persistence) environmentByIDQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 JOIN project_memberships m ON m.project_id = e.project_id
		 JOIN projects p ON p.id = e.project_id
		 WHERE e.id = $1 AND m.user_id = $2
		   AND m.role IN ('owner', 'editor', 'viewer') AND p.kind = $3`,
		environmentID, userID, string(ProjectKindUser))
	return scanEnvironmentRow(row)
}

func (s *persistence) authorizeEnvironmentWriteQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 JOIN project_memberships m ON m.project_id = e.project_id
		 JOIN projects p ON p.id = e.project_id
		 WHERE e.id = $1 AND m.user_id = $2
		   AND m.role IN ('owner', 'editor') AND p.kind = $3`,
		environmentID, userID, string(ProjectKindUser))
	return scanEnvironmentRow(row)
}

func scanEnvironmentRow(scanner interface{ Scan(...any) error }) (EnvironmentRecord, error) {
	var rec EnvironmentRecord
	var kind string
	var networkIdentity int64
	if err := scanner.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &kind, &rec.IsProduction,
		&networkIdentity, &rec.CopiedFromEnvironmentID, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return EnvironmentRecord{}, err
	}
	if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
		return EnvironmentRecord{}, fmt.Errorf("environment %s has invalid network identity %d", rec.ID, networkIdentity)
	}
	rec.NetworkIdentity = uint32(networkIdentity)
	rec.Kind = EnvironmentKind(kind)
	return rec, nil
}

func (s *persistence) duplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (EnvironmentRecord, error) {
	var duplicate EnvironmentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		source, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, sourceEnvironmentID)
		if err != nil {
			return err
		}
		duplicate, err = s.createEnvironmentQuerier(ctx, tx, source.ProjectID, name, false, source.ID)
		if err != nil {
			return err
		}

		type volumeCopy struct {
			name string
			size int64
		}
		var volumes []volumeCopy
		rows, err := tx.QueryContext(ctx, `SELECT name, size_bytes FROM volumes
			WHERE environment_id = $1 ORDER BY created_at, id`, source.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var volume volumeCopy
			if err := rows.Scan(&volume.name, &volume.size); err != nil {
				rows.Close()
				return err
			}
			volumes = append(volumes, volume)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, volume := range volumes {
			if _, err := s.createVolumeTx(ctx, tx, userID, duplicate.ID, volume.name, volume.size); err != nil {
				return err
			}
		}

		type serviceCopy struct {
			name string
			raw  []byte
		}
		var services []serviceCopy
		serviceRows, err := tx.QueryContext(ctx, `SELECT s.name, r.spec_json
			FROM services s JOIN service_revisions r
			  ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			WHERE s.environment_id = $1 ORDER BY s.created_at, s.id`, source.ID)
		if err != nil {
			return err
		}
		for serviceRows.Next() {
			var service serviceCopy
			if err := serviceRows.Scan(&service.name, &service.raw); err != nil {
				serviceRows.Close()
				return err
			}
			services = append(services, service)
		}
		if err := serviceRows.Close(); err != nil {
			return err
		}
		for _, service := range services {
			spec := &platformv1.ServiceSpec{}
			if err := protojson.Unmarshal(service.raw, spec); err != nil {
				return err
			}
			spec = proto.Clone(spec).(*platformv1.ServiceSpec)
			if !copyVariables && spec.GetRuntime() != nil {
				spec.Runtime.Env = nil
			}
			if _, err := s.createStagedServiceTx(ctx, tx, duplicate, service.name, spec, userID); err != nil {
				return err
			}
		}
		return nil
	})
	return duplicate, err
}

func ScanEnvironmentRow(scanner interface{ Scan(...any) error }) (EnvironmentRecord, error) {
	return scanEnvironmentRow(scanner)
}
