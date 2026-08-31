package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var errProductionEnvironment = errors.New("production environment cannot be deleted")

func (s *Store) createEnvironmentQuerier(ctx context.Context, q serviceQueryer, projectID, name string, production bool, copiedFrom string) (environmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return environmentRecord{}, fmt.Errorf("environment name is required")
	}
	networkIdentity, err := allocateEnvironmentNetworkIdentity(ctx, q)
	if err != nil {
		return environmentRecord{}, err
	}
	now := time.Now().UTC()
	rec := environmentRecord{
		ID:                      mustID(),
		ProjectID:               projectID,
		Name:                    name,
		Kind:                    environmentKindPersistent,
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
		return environmentRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureProductionEnvironmentQuerier(ctx context.Context, q serviceQueryer, projectID string) (environmentRecord, error) {
	rec, err := scanEnvironmentRow(q.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return environmentRecord{}, err
	}
	return s.createEnvironmentQuerier(ctx, q, projectID, "Production", true, "")
}

func (s *Store) createEnvironment(ctx context.Context, userID, projectID, name string) (environmentRecord, error) {
	var rec environmentRecord
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

func (s *Store) listEnvironments(ctx context.Context, userID, projectID string) ([]environmentRecord, error) {
	if _, err := s.projectByID(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1
		 ORDER BY e.is_production DESC, e.created_at ASC, e.id ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []environmentRecord
	for rows.Next() {
		rec, err := scanEnvironmentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

const environmentSelect = `SELECT e.id, e.project_id, e.name, e.kind, e.is_production,
	e.network_identity, COALESCE(e.copied_from_environment_id, ''), e.created_at, e.updated_at
	FROM environments e`

func (s *Store) environmentByID(ctx context.Context, userID, environmentID string) (environmentRecord, error) {
	return s.environmentByIDQuerier(ctx, s.db, userID, environmentID)
}

func (s *Store) environmentByIDQuerier(ctx context.Context, q serviceQueryer, userID, environmentID string) (environmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 JOIN project_memberships m ON m.project_id = e.project_id
		 JOIN projects p ON p.id = e.project_id
		 WHERE e.id = $1 AND m.user_id = $2
		   AND m.role IN ('owner', 'editor', 'viewer') AND p.kind = $3`,
		environmentID, userID, string(projectKindUser))
	return scanEnvironmentRow(row)
}

func (s *Store) environmentByIDInternalQuerier(ctx context.Context, q serviceQueryer, environmentID string) (environmentRecord, error) {
	return scanEnvironmentRow(q.QueryRowContext(ctx, environmentSelect+` WHERE e.id = $1`, environmentID))
}

func (s *Store) productionEnvironmentByProjectInternal(ctx context.Context, projectID string) (environmentRecord, error) {
	return scanEnvironmentRow(s.db.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
}

func (s *Store) authorizeEnvironmentWrite(ctx context.Context, userID, environmentID string) (environmentRecord, error) {
	return s.authorizeEnvironmentWriteQuerier(ctx, s.db, userID, environmentID)
}

func (s *Store) authorizeEnvironmentWriteQuerier(ctx context.Context, q serviceQueryer, userID, environmentID string) (environmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 JOIN project_memberships m ON m.project_id = e.project_id
		 JOIN projects p ON p.id = e.project_id
		 WHERE e.id = $1 AND m.user_id = $2
		   AND m.role IN ('owner', 'editor') AND p.kind = $3`,
		environmentID, userID, string(projectKindUser))
	return scanEnvironmentRow(row)
}

func (s *Store) authorizeProjectWriteQuerier(ctx context.Context, q serviceQueryer, userID, projectID string) error {
	var allowed bool
	return q.QueryRowContext(ctx, `SELECT TRUE FROM projects p
		JOIN project_memberships m ON m.project_id = p.id
		WHERE p.id = $1 AND m.user_id = $2
		  AND m.role IN ('owner', 'editor') AND p.kind = $3`,
		projectID, userID, string(projectKindUser)).Scan(&allowed)
}

func scanEnvironmentRow(scanner interface{ Scan(...any) error }) (environmentRecord, error) {
	var rec environmentRecord
	var kind string
	var networkIdentity int64
	if err := scanner.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &kind, &rec.IsProduction,
		&networkIdentity, &rec.CopiedFromEnvironmentID, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return environmentRecord{}, err
	}
	if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
		return environmentRecord{}, fmt.Errorf("environment %s has invalid network identity %d", rec.ID, networkIdentity)
	}
	rec.NetworkIdentity = uint32(networkIdentity)
	rec.Kind = environmentKind(kind)
	return rec, nil
}

func (s *Store) renameEnvironment(ctx context.Context, userID, environmentID, name string) (environmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return environmentRecord{}, fmt.Errorf("environment name is required")
	}
	var rec environmentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
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
		rec, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
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
		return s.bumpAllDesiredRevisionsTx(ctx, tx)
	})
	if err != nil || !identityCatalogChanged {
		return agentIDs, err
	}
	agentIDs, err = s.agentIDs(ctx)
	return agentIDs, err
}

func (s *Store) duplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (environmentRecord, error) {
	var duplicate environmentRecord
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
			if _, err := s.createStagedServiceTx(ctx, tx, duplicate, service.name, spec); err != nil {
				return err
			}
		}
		return nil
	})
	return duplicate, err
}

func (s *Store) deployEnvironment(ctx context.Context, userID, environmentID string) ([]serviceRecord, []string, error) {
	var (
		serviceIDs             []string
		agentIDs               []string
		identityCatalogChanged bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		serviceIDs = nil
		agentIDs = nil
		identityCatalogChanged = false
		environment, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.id
			FROM services s
			LEFT JOIN service_rollouts r
			  ON r.service_id = s.id AND r.rollout_generation = s.current_rollout_generation
			WHERE s.environment_id = $1
			  AND (s.current_rollout_generation = 0 OR r.spec_revision IS DISTINCT FROM s.current_spec_revision)
			ORDER BY s.id FOR UPDATE OF s`, environment.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			serviceIDs = append(serviceIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, serviceID := range serviceIDs {
			service, err := s.serviceByIDQuerier(ctx, tx, userID, "", serviceID)
			if err != nil {
				return err
			}
			if volumeName := serviceVolumeName(service.Spec); volumeName != "" {
				if err := s.requireVolumeQuerier(ctx, tx, environment.ID, volumeName); err != nil {
					return err
				}
			}
			if source := desiredSourceSpec(service.Spec); source != nil {
				var granted bool
				err := tx.QueryRowContext(ctx, `SELECT EXISTS(
					SELECT 1 FROM project_github_repositories
					WHERE project_id = $1 AND lower(full_name) = lower($2)
				)`, environment.ProjectID, source.GetRepositorySelector()).Scan(&granted)
				if err != nil {
					return err
				}
				if !granted {
					return fmt.Errorf("repository %q is not granted to project %s", source.GetRepositorySelector(), environment.ProjectID)
				}
			}
			if service.AllocatedAgentID == "" {
				identityCatalogChanged = true
			}
			needsSourceBuild, err := s.serviceNeedsSourceBuildTx(ctx, tx, service)
			if err != nil {
				return err
			}
			deployed, _, err := s.redeployServiceTx(ctx, tx, userID, "", serviceID)
			if err != nil {
				return err
			}
			if needsSourceBuild {
				if err := s.enqueueSourceSpecChangedTx(ctx, tx, service.ID, service.SpecRevision, false); err != nil {
					return err
				}
			}
			if deployed.AllocatedAgentID != "" {
				agentIDs = append(agentIDs, deployed.AllocatedAgentID)
			}
		}
		if identityCatalogChanged {
			return s.bumpAllDesiredRevisionsTx(ctx, tx)
		}
		return s.bumpDesiredRevisionsTx(ctx, tx, agentIDs)
	})
	if err != nil {
		return nil, nil, err
	}
	if identityCatalogChanged {
		agentIDs, err = s.agentIDs(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	services := make([]serviceRecord, 0, len(serviceIDs))
	for _, id := range serviceIDs {
		service, err := s.serviceByID(ctx, userID, "", id)
		if err != nil {
			return nil, nil, err
		}
		services = append(services, service)
	}
	return services, agentIDs, nil
}

// serviceNeedsSourceBuildTx distinguishes source changes from runtime-only
// changes. A source-backed service needs a build for its first rollout and when
// its repository/ref/build recipe changes. Runtime, restart, and replica-only
// revisions reuse the image already resolved by the deployed rollout.
func (s *Store) serviceNeedsSourceBuildTx(ctx context.Context, tx *sql.Tx, service serviceRecord) (bool, error) {
	if desiredSourceSpec(service.Spec) == nil {
		return false, nil
	}
	if service.RolloutGeneration == 0 || strings.TrimSpace(service.ResolvedImage) == "" {
		return true, nil
	}

	var deployedSpecRevision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spec_revision FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&deployedSpecRevision); err != nil {
		return false, err
	}
	deployedSpec, err := s.loadServiceDetailsQuerier(ctx, tx, service.ID, deployedSpecRevision)
	if err != nil {
		return false, err
	}
	return !sameDesiredSourceSpec(deployedSpec, service.Spec), nil
}
