package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/source"
)

// ReadModel restricts persistence consumers to delivery reads. Delivery itself
// implements it, so reads and commands share the same live state and ownership.
type ReadModel interface {
	// ListAgents requires platform-operator membership.
	ListAgents(context.Context, authz.User) ([]AgentRecord, error)
	ListServiceDeployments(context.Context, authz.User, string, int32) ([]DeploymentRecord, error)
	ListDomainBindings(context.Context, authz.User, string, bool) ([]DomainBindingRecord, error)
	ServiceStatus(context.Context, authz.User, string) (ServiceRecord, []AllocationRecord, error)
	EnvironmentByID(context.Context, authz.User, string) (EnvironmentRecord, error)
	ListServices(context.Context, authz.User, string, bool) ([]ServiceRecord, error)
	ServiceByID(context.Context, authz.User, string) (ServiceRecord, error)
	// AgentByID, AgentIDs, BuildByID, ServiceSnapshot, and ListAllocationsByServiceID are
	// system reads without user authorization, for reconciliation and builder paths.
	AgentByID(context.Context, string) (AgentRecord, error)
	AgentIDs(context.Context) ([]string, error)
	ListAllocationsByServiceID(context.Context, string) ([]AllocationRecord, error)
	BuildByID(context.Context, string) (BuildRunRecord, error)
	ServiceSnapshot(context.Context, string) (ServiceRecord, error)
}

func (d *Delivery) ListAgents(ctx context.Context, user authz.User) ([]AgentRecord, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return nil, err
	}
	return d.store.listAgents(ctx)
}

func (d *Delivery) AgentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	return d.store.agentByID(ctx, agentID)
}

func (d *Delivery) AgentIDs(ctx context.Context) ([]string, error) { return d.store.agentIDs(ctx) }

func (d *Delivery) ListServiceDeployments(ctx context.Context, user authz.User, serviceID string, limit int32) ([]DeploymentRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	return d.store.listServiceDeployments(ctx, scope, limit)
}

func (d *Delivery) ListDomainBindings(ctx context.Context, user authz.User, serviceID string, includeDeleted bool) ([]DomainBindingRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	return d.store.listDomainBindings(ctx, scope, includeDeleted)
}

func (d *Delivery) ServiceStatus(ctx context.Context, user authz.User, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return d.store.serviceStatus(ctx, scope)
}

func (d *Delivery) EnvironmentByID(ctx context.Context, user authz.User, environmentID string) (EnvironmentRecord, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	return d.store.environmentByID(ctx, scope)
}

func (d *Delivery) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	return d.store.listAllocationsByServiceID(ctx, serviceID)
}

func (d *Delivery) ListServices(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return nil, err
	}
	return d.store.listServices(ctx, scope, includeDeleted)
}

func (d *Delivery) ServiceByID(ctx context.Context, user authz.User, serviceID string) (ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.store.serviceByID(ctx, scope)
}

func (d *Delivery) BuildByID(ctx context.Context, buildID string) (BuildRunRecord, error) {
	return d.store.buildRunByIDQuerier(ctx, d.store.db, buildID)
}

func (d *Delivery) ServiceSnapshot(ctx context.Context, serviceID string) (ServiceRecord, error) {
	return d.store.serviceByIDInternalQuerier(ctx, d.store.db, serviceID)
}

func (s *persistence) listServices(ctx context.Context, scope authz.Environment, includeDeleted bool) ([]ServiceRecord, error) {
	filter := `
		    AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1`+filter+`
		  ORDER BY s.created_at ASC`,
		scope.ID(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ServiceRecord
	for rows.Next() {
		rec, err := scanServiceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		spec, err := s.loadServiceDetails(ctx, out[i].ID, out[i].SpecRevision)
		if err != nil {
			return nil, err
		}
		out[i].Spec = spec
		out[i].SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, s.db, spec, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].LatestBuild, err = s.latestBuildForServiceQuerier(ctx, s.db, out[i].LatestBuildID)
		if err != nil {
			return nil, err
		}
		if err := s.attachLatestDeploymentQuerier(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
		out[i].UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, s.db, out[i].ID, out[i].Spec, out[i].RolloutGeneration)
		if err != nil {
			return nil, err
		}
		out[i].PendingChanges = len(out[i].UnappliedChanges) > 0
	}
	return out, nil
}

func (s *persistence) serviceByID(ctx context.Context, scope authz.Service) (ServiceRecord, error) {
	return s.serviceByIDQuerier(ctx, s.db, scope)
}

func (s *persistence) serviceByIDQuerier(ctx context.Context, q ServiceQueryer, scope authz.Service) (ServiceRecord, error) {
	return s.serviceByRowQuerier(ctx, q, q.QueryRowContext(ctx,
		serviceSelectSQL+`
		  WHERE s.id = $1 AND e.project_id = $2`,
		scope.ID(), scope.ProjectID(),
	))
}

// serviceByIDInEnvironmentQuerier loads a service confined to an authorized
// environment. It serves operations that enumerate services under an
// environment scope, where minting one scope per service would be wasteful.
func (s *persistence) serviceByIDInEnvironmentQuerier(ctx context.Context, q ServiceQueryer, env authz.Environment, serviceID string) (ServiceRecord, error) {
	return s.serviceByRowQuerier(ctx, q, q.QueryRowContext(ctx,
		serviceSelectSQL+`
		  WHERE s.id = $1 AND s.environment_id = $2 AND e.project_id = $3`,
		serviceID, env.ID(), env.ProjectID(),
	))
}

func (s *persistence) serviceByRowQuerier(ctx context.Context, q ServiceQueryer, row *sql.Row) (ServiceRecord, error) {
	rec, err := scanServiceRow(row)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, q, rec.Spec, rec.ID)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return ServiceRecord{}, err
	}
	rec.UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, q, rec.ID, rec.Spec, rec.RolloutGeneration)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.PendingChanges = len(rec.UnappliedChanges) > 0
	return rec, nil
}

func (s *persistence) serviceByNameQuerier(ctx context.Context, q ServiceQueryer, environmentID, name string) (ServiceRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1 AND s.name = $2`,
		environmentID,
		name,
	)
	rec, err := scanServiceRow(row)
	switch {
	case err == nil:
		rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
		if err != nil {
			return ServiceRecord{}, false, err
		}
		return rec, true, nil
	case err == sql.ErrNoRows:
		return ServiceRecord{}, false, nil
	default:
		return ServiceRecord{}, false, err
	}
}

const serviceSelectSQL = `SELECT s.id, s.environment_id, e.project_id, s.name, s.current_spec_revision,
		        COALESCE(ds.current_rollout_generation, 0),
		        COALESCE((SELECT a.agent_id FROM allocations a WHERE a.service_id = s.id AND a.rollout_state <> 'lost' ORDER BY CASE a.rollout_state WHEN 'serving' THEN 0 WHEN 'starting' THEN 1 ELSE 2 END, a.id LIMIT 1), ''),
		        COALESCE(ds.current_artifact_id, ''), COALESCE((SELECT image_ref FROM build_artifacts WHERE id = ds.current_artifact_id), ''),
		        COALESCE(ds.last_successful_commit_sha, ''), COALESCE(ds.latest_build_id, ''),
		        s.desired_replica_count, COALESCE(ds.placement_message, ''), s.created_at, GREATEST(s.updated_at, ds.updated_at),
		        s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM services s
		   JOIN service_delivery_status ds ON ds.service_id = s.id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id`

func scanServiceRow(scanner interface{ Scan(...any) error }) (ServiceRecord, error) {
	var rec ServiceRecord
	var self, environment, project Tombstone
	targets := []any{
		&rec.ID,
		&rec.EnvironmentID,
		&rec.ProjectID,
		&rec.Name,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.AllocatedAgentID,
		&rec.ResolvedArtifactID,
		&rec.ResolvedImage,
		&rec.LastSuccessfulCommitSHA,
		&rec.LatestBuildID,
		&rec.DesiredReplicaCount,
		&rec.PlacementMessage,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	}
	targets = ScanTombstone(targets, &self)
	targets = ScanTombstone(targets, &environment)
	if err := scanner.Scan(ScanTombstone(targets, &project)...); err != nil {
		return ServiceRecord{}, err
	}
	if rec.DesiredReplicaCount <= 0 {
		rec.DesiredReplicaCount = DefaultDesiredReplicaCount
	}
	rec.LatestBuild = nil
	rec.Deletion = EffectiveDeletion(self, environment, project)
	return rec, nil
}

func (s *persistence) loadServiceDetails(ctx context.Context, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	return s.loadServiceDetailsQuerier(ctx, s.db, serviceID, specRevision)
}

// loadServiceDetailsQuerier loads a full spec revision, decrypting its env.
func (s *persistence) loadServiceDetailsQuerier(ctx context.Context, q ServiceQueryer, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	var rawSpec, envCiphertext []byte
	var envDEKID string
	if err := q.QueryRowContext(ctx,
		`SELECT spec_json, COALESCE(env_dek_id, ''), env_ciphertext FROM service_revisions WHERE service_id = $1 AND spec_revision = $2`,
		serviceID, specRevision).Scan(&rawSpec, &envDEKID, &envCiphertext); err != nil {
		return nil, err
	}
	spec, err := LoadServiceSpec(rawSpec)
	if err != nil {
		return nil, err
	}
	env, err := s.decryptRevisionEnv(ctx, q, serviceID, specRevision, envDEKID, envCiphertext)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		if spec.Runtime == nil {
			spec.Runtime = &platformv1.ServiceRuntime{}
		}
		spec.Runtime.Env = env
	}
	return spec, nil
}

const environmentSelect = `SELECT e.id, e.project_id, e.name, e.kind, e.is_production, e.auto_deploy,
	e.network_identity, COALESCE(e.copied_from_environment_id, ''), e.created_at, e.updated_at,
	e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
	p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
	FROM environments e JOIN projects p ON p.id = e.project_id`

func (s *persistence) environmentByID(ctx context.Context, scope authz.Environment) (EnvironmentRecord, error) {
	return s.environmentByIDQuerier(ctx, s.db, scope)
}

func (s *persistence) environmentByIDQuerier(ctx context.Context, q ServiceQueryer, scope authz.Environment) (EnvironmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2`,
		scope.ID(), scope.ProjectID())
	return scanEnvironmentRow(row)
}

func scanEnvironmentRow(scanner interface{ Scan(...any) error }) (EnvironmentRecord, error) {
	var rec EnvironmentRecord
	var kind string
	var networkIdentity int64
	var self, project Tombstone
	targets := []any{&rec.ID, &rec.ProjectID, &rec.Name, &kind, &rec.IsProduction, &rec.AutoDeploy,
		&networkIdentity, &rec.CopiedFromEnvironmentID, &rec.CreatedAt, &rec.UpdatedAt}
	targets = ScanTombstone(targets, &self)
	if err := scanner.Scan(ScanTombstone(targets, &project)...); err != nil {
		return EnvironmentRecord{}, err
	}
	if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
		return EnvironmentRecord{}, fmt.Errorf("environment %s has invalid network identity %d", rec.ID, networkIdentity)
	}
	rec.NetworkIdentity = uint32(networkIdentity)
	rec.Kind = EnvironmentKind(kind)
	rec.Deletion = EffectiveDeletion(self, project)
	return rec, nil
}

func (s *persistence) duplicateEnvironment(ctx context.Context, scope authz.Environment, name string, copyVariables bool) (EnvironmentRecord, error) {
	var duplicate EnvironmentRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		source, err := s.environmentByIDQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if source.Deletion != nil {
			return ErrEnvironmentDeleted
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
			WHERE environment_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, source.ID)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, volume := range volumes {
			if _, err := s.createVolumeTx(ctx, tx, scope.Project(), duplicate.ID, volume.name, volume.size); err != nil {
				return err
			}
		}

		type serviceCopy struct {
			id, name     string
			specRevision int64
		}
		var services []serviceCopy
		serviceRows, err := tx.QueryContext(ctx, `SELECT id, name, current_spec_revision
			FROM services WHERE environment_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, source.ID)
		if err != nil {
			return err
		}
		for serviceRows.Next() {
			var service serviceCopy
			if err := serviceRows.Scan(&service.id, &service.name, &service.specRevision); err != nil {
				serviceRows.Close()
				return err
			}
			services = append(services, service)
		}
		if err := serviceRows.Err(); err != nil {
			serviceRows.Close()
			return err
		}
		if err := serviceRows.Close(); err != nil {
			return err
		}
		// Copied env is re-encrypted under the duplicate environment's own key.
		for _, service := range services {
			spec, err := s.loadServiceDetailsQuerier(ctx, tx, service.id, service.specRevision)
			if err != nil {
				return err
			}
			if !copyVariables && spec.GetRuntime() != nil {
				spec.Runtime.Env = nil
			}
			if _, err := s.createStagedServiceTx(ctx, tx, duplicate, service.name, spec, scope.UserID()); err != nil {
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

// requireVolumeAttachableQuerier checks that volumeName names a live volume
// in the environment that no other live service mounts, either in its draft
// or in its deployed revision. A volume belongs to at most one service: two
// services on different nodes would otherwise each see their own copy.
func (s *persistence) requireVolumeAttachableQuerier(ctx context.Context, q ServiceQueryer, environmentID, serviceID, volumeName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM volumes WHERE environment_id = $1 AND name = $2 AND deleted_at IS NULL`, environmentID, volumeName).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", ErrVolumeNotFound, volumeName)
	case err != nil:
		return err
	}
	owner, err := VolumeAttachmentOwner(ctx, q, environmentID, volumeName, serviceID)
	if err != nil {
		return err
	}
	if owner != "" {
		return fmt.Errorf("%w: volume %q is mounted by service %s", ErrVolumeAttached, volumeName, owner)
	}
	return nil
}

// VolumeAttachmentOwner returns the name of a live service other than
// exceptServiceID whose draft or deployed revision mounts volumeName.
func VolumeAttachmentOwner(ctx context.Context, q ServiceQueryer, environmentID, volumeName, exceptServiceID string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT svc.id, svc.name, draft.spec_json, deployed.spec_json
		  FROM live_services svc
		  JOIN service_revisions draft ON draft.service_id = svc.id AND draft.spec_revision = svc.current_spec_revision
		  LEFT JOIN service_delivery_status ds ON ds.service_id = svc.id
		  LEFT JOIN service_rollouts ro ON ro.service_id = svc.id AND ro.rollout_generation = ds.current_rollout_generation
		  LEFT JOIN service_revisions deployed ON deployed.service_id = ro.service_id AND deployed.spec_revision = ro.spec_revision
		 WHERE svc.environment_id = $1 AND svc.id <> $2
		 ORDER BY svc.created_at, svc.id`, environmentID, exceptServiceID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var draftJSON, deployedJSON []byte
		if err := rows.Scan(&id, &name, &draftJSON, &deployedJSON); err != nil {
			return "", err
		}
		for _, raw := range [][]byte{draftJSON, deployedJSON} {
			if len(raw) == 0 {
				continue
			}
			spec, err := LoadServiceSpec(raw)
			if err != nil {
				return "", err
			}
			if ServiceVolumeName(spec) == volumeName {
				return name, nil
			}
		}
	}
	return "", rows.Err()
}

func (s *persistence) listDomainBindings(ctx context.Context, scope authz.Service, includeDeleted bool) ([]DomainBindingRecord, error) {
	filter := `
		    AND d.deleted_at IS NULL AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.hostname, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at,
		d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
		s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		FROM domain_bindings d
		JOIN services s ON s.id = d.service_id
		JOIN environments e ON e.id = s.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE d.service_id = $1`+filter+`
		ORDER BY d.hostname ASC`, scope.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DomainBindingRecord
	for rows.Next() {
		var binding DomainBindingRecord
		var self, service, environment, project Tombstone
		binding.ProjectID = scope.ProjectID()
		binding.EnvironmentID = scope.EnvironmentID()
		targets := []any{&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt}
		targets = ScanTombstone(targets, &self)
		targets = ScanTombstone(targets, &service)
		targets = ScanTombstone(targets, &environment)
		if err := rows.Scan(ScanTombstone(targets, &project)...); err != nil {
			return nil, err
		}
		binding.Deletion = EffectiveDeletion(self, service, environment, project)
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *persistence) domainTargetPortsForService(ctx context.Context, serviceID string) ([]int32, error) {
	return domainTargetPortsForServiceQuerier(ctx, s.db, serviceID)
}

func domainTargetPortsForServiceQuerier(ctx context.Context, q ServiceQueryer, serviceID string) ([]int32, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT target_port
		   FROM domain_bindings
		  WHERE service_id = $1
		  ORDER BY hostname ASC`,
		serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ports []int32
	for rows.Next() {
		var port int32
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, rows.Err()
}

func (s *persistence) serviceStatus(ctx context.Context, scope authz.Service) (ServiceRecord, []AllocationRecord, error) {
	service, err := s.serviceByID(ctx, scope)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocs, err := s.listAllocationsByServiceID(ctx, scope.ID())
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return service, allocs, nil
}

func (s *persistence) listServiceDeployments(ctx context.Context, scope authz.Service, limit int32) ([]DeploymentRecord, error) {
	queryLimit := int(limit)
	if queryLimit <= 0 {
		queryLimit = 50
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1
		  ORDER BY created_at DESC, rollout_generation DESC, id DESC
		  LIMIT $2`,
		scope.ID(), queryLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []DeploymentRecord
	for rows.Next() {
		rec, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range records {
		transitions, err := s.loadDeploymentTransitions(ctx, s.db, records[i].ID)
		if err != nil {
			return nil, err
		}
		records[i].Transitions = transitions
		records[i].Actions, err = s.loadDeploymentActions(ctx, s.db, records[i].ID)
		if err != nil {
			return nil, err
		}
		if records[i].BuildID != "" {
			build, err := s.buildRunByIDQuerier(ctx, s.db, records[i].BuildID)
			if err != nil {
				return nil, err
			}
			buildCopy := build
			records[i].Build = &buildCopy
		}
		if records[i].ArtifactID != "" {
			artifact, err := s.buildArtifactByIDQuerier(ctx, s.db, records[i].ArtifactID)
			if err != nil {
				return nil, err
			}
			artifactCopy := artifact
			records[i].Artifact = &artifactCopy
		}
	}
	return records, nil
}

func (s *persistence) loadServiceSourceSummaryQuerier(ctx context.Context, q ServiceQueryer, spec *platformv1.ServiceSpec, serviceID string) (*platformv1.ServiceSourceSummary, error) {
	if spec == nil || spec.GetSource() == nil {
		return nil, nil
	}
	if image := spec.GetSource().GetImage(); image != nil {
		return BuildSourceSummary(spec), nil
	}
	desired := source.DesiredSourceSpec(spec)
	if desired == nil {
		return nil, nil
	}
	binding, err := s.sourceStore.SourceBindingByServiceIDQuerier(ctx, q, serviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, nil, nil, nil), nil
	case err != nil:
		return nil, err
	}
	revision, err := s.sourceStore.LatestSourceRevisionByBindingIDTx(ctx, q, binding.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, nil, nil), nil
	case err != nil:
		return nil, err
	}
	snapshot, err := s.sourceStore.SourceSnapshotByRevisionIDTx(ctx, q, revision.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, &revision, nil), nil
	case err != nil:
		return nil, err
	default:
		return toProtoSourceStateSummary(desired, &binding, &revision, &snapshot), nil
	}
}

func (s *persistence) enqueueSourceSpecChangedTx(ctx context.Context, tx *sql.Tx, serviceID string, specRevision int64, force bool) error {
	if serviceID == "" {
		return errors.New("service id is required")
	}
	_, err := s.enqueueSourceWorkItemTx(ctx, tx, source.SourceSpecChangedParams(serviceID, specRevision, force))
	return err
}
