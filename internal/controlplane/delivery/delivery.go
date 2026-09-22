package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/source"
)

type Delivery struct {
	schedulerMu    sync.Mutex
	store          *persistence
	live           *Live
	notifier       PlatformNotifier
	ingress        PlatformIngress
	events         Events
	logEmitter     *logs.LogEmitter
	rolloutNow     func() time.Time
	failoverNow    func() time.Time
	buildScheduler BuildSchedulerConfig
	allocSync      *allocSync
	imageResolver  registry.ImageResolver
}

func (d *Delivery) BuildSchedulerConfig() BuildSchedulerConfig {
	if d == nil {
		return DefaultBuildSchedulerConfig()
	}
	return d.buildScheduler.WithDefaults()
}

func (d *Delivery) SetBuildSchedulerConfigForTest(cfg BuildSchedulerConfig) {
	if d == nil {
		return
	}
	d.buildScheduler = cfg.WithDefaults()
}

// SetImageResolver installs the direct-image tag resolver. Production
// wires the HTTP resolver; tests install a static one. Nil resolves
// digest-pinned references only.
func (d *Delivery) SetImageResolver(resolver registry.ImageResolver) {
	if d == nil {
		return
	}
	d.imageResolver = resolver
}

type ReleasedService struct {
	Service     ServiceRecord
	Allocations []AllocationRecord
}

type DeploymentActionResult struct {
	Service     ServiceRecord
	Allocations []AllocationRecord
	EventIndex  int64
}

func (d *Delivery) ApplyDeploymentAction(ctx context.Context, user authz.User, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (DeploymentActionResult, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return DeploymentActionResult{}, err
	}
	service, _, agentIDs, err := d.applyDeploymentAction(ctx, scope, deploymentID, action, idempotencyKey, allocationID)
	if err != nil {
		return DeploymentActionResult{}, err
	}
	for _, agentID := range agentIDs {
		if d.notifier != nil {
			d.notifier.Notify(agentID)
		}
	}
	if d.ingress != nil {
		d.ingress.RequestSync()
	}
	service, allocations, err := d.store.serviceStatus(ctx, scope)
	if err != nil {
		return DeploymentActionResult{}, err
	}
	var eventIndex int64
	if d.events != nil {
		eventIndex, err = d.events.Current(ctx)
		if err != nil {
			return DeploymentActionResult{}, err
		}
	}
	return DeploymentActionResult{Service: service, Allocations: allocations, EventIndex: eventIndex}, nil
}

func (d *Delivery) ReleaseEnvironment(ctx context.Context, user authz.User, environmentID string) ([]ReleasedService, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return nil, err
	}
	// Direct-image tags resolve outside the product transaction and
	// outside the scheduler lock: registry calls must never hold product
	// locks, and a slow or unreachable registry must not stall other
	// scheduler-serialized mutations (rollout, failover, agent status,
	// deployment actions) while pins are fetched. Only the services this
	// release selects are resolved, so an unchanged service's stale tag
	// cannot block unrelated pending changes. A spec racing the pre-read
	// retries with a fresh map: the release transaction re-verifies each
	// input before using its pre-resolved digest.
	var services []ReleasedService
	var errRelease error
	for attempt := 0; attempt < 3; attempt++ {
		inputs, err := d.store.pendingDirectImageInputs(ctx, scope)
		if err != nil {
			return nil, err
		}
		resolved, err := d.resolveDirectImages(ctx, inputs)
		if err != nil {
			return nil, err
		}
		services, errRelease = d.releaseEnvironmentTx(ctx, scope, resolved)
		if !errors.Is(errRelease, errDirectImageChanged) {
			break
		}
	}
	if errRelease != nil {
		return nil, fmt.Errorf("release environment: %w", errRelease)
	}
	return services, nil
}

func (d *Delivery) releaseEnvironmentTx(ctx context.Context, scope authz.Environment, resolved map[string]resolvedDirectImage) ([]ReleasedService, error) {
	// The scheduler lock serializes the mutation phase only; resolution
	// and pre-reads run outside it (see ReleaseEnvironment).
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	var services []ReleasedService
	var (
		serviceIDs             []string
		agentIDs               []string
		identityCatalogChanged bool
	)
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		services = nil
		serviceIDs = nil
		agentIDs = nil
		identityCatalogChanged = false
		environment, err := d.store.environmentByIDQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if environment.Deletion != nil {
			return ErrEnvironmentDeleted
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.id
			FROM services s
			JOIN service_delivery_status ds ON ds.service_id = s.id
			LEFT JOIN service_rollouts r
			  ON r.service_id = s.id AND r.rollout_generation = ds.current_rollout_generation
			WHERE s.environment_id = $1
			  AND s.deleted_at IS NULL
			  AND (ds.current_rollout_generation IS NULL OR r.spec_revision IS DISTINCT FROM s.current_spec_revision)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !environment.AutoDeploy {
			pending, err := d.store.sourceStore.ServicesWithUnbuiltSourceRevisionsTx(ctx, tx, environment.ID)
			if err != nil {
				return err
			}
			for _, id := range pending {
				if !slices.Contains(serviceIDs, id) {
					serviceIDs = append(serviceIDs, id)
				}
			}
			slices.Sort(serviceIDs)
		}
		for _, serviceID := range serviceIDs {
			service, err := d.store.serviceByIDInEnvironmentQuerier(ctx, tx, scope, serviceID)
			if err != nil {
				return err
			}
			if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
				if err := d.store.requireVolumeQuerier(ctx, tx, environment.ID, volumeName); err != nil {
					return err
				}
			}
			if desired := source.DesiredSourceSpec(service.Spec); desired != nil {
				var granted bool
				err := tx.QueryRowContext(ctx, `SELECT EXISTS(
					SELECT 1 FROM project_github_repositories
					WHERE project_id = $1 AND lower(full_name) = lower($2)
				)`, environment.ProjectID, desired.GetRepositorySelector()).Scan(&granted)
				if err != nil {
					return err
				}
				if !granted {
					return fmt.Errorf("repository %q is not granted to project %s", desired.GetRepositorySelector(), environment.ProjectID)
				}
			}
			if service.AllocatedAgentID == "" {
				identityCatalogChanged = true
			}
			needsSourceBuild, err := d.serviceNeedsSourceBuildTx(ctx, tx, service, environment.AutoDeploy)
			if err != nil {
				return err
			}
			released, err := d.releaseServiceRevisionTx(ctx, tx, scope, serviceID, resolved)
			if err != nil {
				return err
			}
			if needsSourceBuild {
				if err := d.store.enqueueSourceSpecChangedTx(ctx, tx, service.ID, service.SpecRevision, false); err != nil {
					return err
				}
			}
			if released.AllocatedAgentID != "" {
				agentIDs = append(agentIDs, released.AllocatedAgentID)
			}
		}
		if identityCatalogChanged {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM agents ORDER BY id`)
			if err != nil {
				return err
			}
			agentIDs = nil
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				agentIDs = append(agentIDs, id)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		for _, id := range serviceIDs {
			service, err := d.store.serviceByIDInEnvironmentQuerier(ctx, tx, scope, id)
			if err != nil {
				return err
			}
			allocations, err := d.store.listAllocationsByServiceIDQuerier(ctx, tx, id, false)
			if err != nil {
				return err
			}
			services = append(services, ReleasedService{Service: service, Allocations: allocations})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, agentID := range agentIDs {
		if d.notifier != nil {
			d.notifier.Notify(agentID)
		}
	}
	return services, nil
}

func (d *Delivery) serviceNeedsSourceBuildTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, autoDeploy bool) (bool, error) {
	if source.DesiredSourceSpec(service.Spec) == nil {
		return false, nil
	}
	if service.RolloutGeneration == 0 || strings.TrimSpace(service.ResolvedArtifactID) == "" {
		return true, nil
	}
	if !autoDeploy {
		pending, err := d.store.sourceStore.ServiceHasUnbuiltSourceRevisionTx(ctx, tx, service.ID)
		if err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
	}

	var deployedSpecRevision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spec_revision FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		service.ID, service.RolloutGeneration,
	).Scan(&deployedSpecRevision); err != nil {
		return false, err
	}
	deployedSpec, err := d.store.loadServiceDetailsQuerier(ctx, tx, service.ID, deployedSpecRevision)
	if err != nil {
		return false, err
	}
	return !sameDesiredSourceSpec(deployedSpec, service.Spec), nil
}

type replacementRolloutBump struct {
	SpecRevision  int64
	Replicas      int32
	ArtifactID    string
	BuildID       *string
	RolloutReason string
	UserID        string
}

func (d *Delivery) beginReplacementRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, now time.Time) ([]AllocationRecord, error) {
	existing, err := d.store.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	if _, err := d.prepareReplacementRolloutTx(ctx, tx, service, existing, now); err != nil {
		return nil, err
	}
	return existing, nil
}

func (d *Delivery) bumpServiceRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, bump replacementRolloutBump, now time.Time) (int64, error) {
	nextRollout := service.RolloutGeneration + 1
	buildID := sql.NullString{}
	buildIDValue := ""
	if bump.BuildID != nil {
		buildID = sql.NullString{String: *bump.BuildID, Valid: true}
		buildIDValue = *bump.BuildID
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE service_delivery_status AS ds
		    SET current_rollout_generation = $1,
		        current_artifact_id = NULLIF($2, ''),
		        latest_build_id = CASE WHEN $3::text IS NULL THEN latest_build_id ELSE NULLIF($3::text, '') END,
		        updated_at = $4
		  WHERE service_id = $5 AND COALESCE(current_rollout_generation, 0) = $7
		    AND EXISTS (SELECT 1 FROM services s WHERE s.id = ds.service_id AND s.current_spec_revision = $6)`,
		nextRollout, bump.ArtifactID, buildID, now,
		service.ID, service.SpecRevision, service.RolloutGeneration,
	)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrConcurrentUpdate
	}
	if _, err := tx.ExecContext(ctx, `UPDATE services SET current_spec_revision = $1, desired_replica_count = $2, updated_at = $3 WHERE id = $4`, bump.SpecRevision, bump.Replicas, now, service.ID); err != nil {
		return 0, err
	}
	journal.RecordService(ctx, service.ID)
	if err := d.store.insertServiceRolloutTx(ctx, tx, service.ID, nextRollout, bump.SpecRevision, bump.RolloutReason, buildIDValue, bump.UserID, now); err != nil {
		return 0, err
	}
	return nextRollout, nil
}

func (d *Delivery) releaseServiceRevisionTx(ctx context.Context, tx *sql.Tx, env authz.Environment, serviceID string, resolved map[string]resolvedDirectImage) (ServiceRecord, error) {

	current, err := d.store.serviceByIDInEnvironmentQuerier(ctx, tx, env, serviceID)
	if err != nil {
		return ServiceRecord{}, err
	}
	// The release enumeration locks live services only; lock and re-check so
	// the unbuilt-revision backfill and concurrent deletes cannot slip a
	// release past a tombstone.
	locked, err := d.store.lockServiceDeletionTx(ctx, tx, current.ID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if err := requireLiveService(locked); err != nil {
		return ServiceRecord{}, err
	}
	if current.DesiredReplicaCount <= 0 {
		current.DesiredReplicaCount = DefaultDesiredReplicaCount
	}
	if specHasDesiredReplicaCount(current.Spec) {
		desired := current.Spec.GetDesiredReplicaCount()
		if err := validateDesiredReplicaCount(desired); err != nil {
			return ServiceRecord{}, err
		}
		if err := validateVolumeReplicaCompatibility(current.Spec, desired); err != nil {
			return ServiceRecord{}, err
		}
		current.DesiredReplicaCount = desired
	}
	now := time.Now().UTC()
	if _, err := d.beginReplacementRolloutTx(ctx, tx, current, now); err != nil {
		return ServiceRecord{}, err
	}
	artifactID := current.ResolvedArtifactID
	if directImage := directImageRef(current.Spec); directImage != "" {
		artifact, err := d.directImageArtifactTx(ctx, tx, current.ID, directImage, resolved[current.ID], deploymentActor{Kind: DeploymentCauseUser, ID: env.UserID()}, now)
		if err != nil {
			return ServiceRecord{}, err
		}
		artifactID = artifact.ID
	}
	nextRolloutGeneration, err := d.bumpServiceRolloutTx(ctx, tx, current, replacementRolloutBump{
		SpecRevision: current.SpecRevision, Replicas: current.DesiredReplicaCount,
		ArtifactID: artifactID, RolloutReason: "environment_release", UserID: env.UserID(),
	}, now)
	if err != nil {
		return ServiceRecord{}, err
	}
	releaseState := DeploymentStateScheduling
	releaseDetail := "Environment release scheduled"
	if source.DesiredSourceSpec(current.Spec) != nil && artifactID == "" {
		releaseState = DeploymentStateStaged
		releaseDetail = "Environment release waiting for source build"
	}
	dep, err := d.store.insertDeploymentTx(ctx, tx, serviceID, releaseState, deploymentActor{Kind: DeploymentCauseUser, ID: env.UserID()}, reasonEnvironmentRelease, releaseDetail, current.SpecRevision, nextRolloutGeneration, "", artifactID, env.UserID(), now)
	if err != nil {
		return ServiceRecord{}, err
	}
	current.LatestDeployment = &dep
	current.RolloutGeneration = nextRolloutGeneration
	current.ResolvedArtifactID = artifactID
	current.ResolvedImage = dep.ImageDigest
	current.PendingChanges = false
	current.UpdatedAt = now
	if _, err := d.advanceRolloutTx(ctx, tx, serviceID, now); err != nil {
		return ServiceRecord{}, err
	}
	return current, nil
}

func (d *Delivery) DuplicateEnvironment(ctx context.Context, user authz.User, sourceEnvironmentID, name string, copyVariables bool) (EnvironmentRecord, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, sourceEnvironmentID, authz.Write)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	return d.store.duplicateEnvironment(ctx, scope, name, copyVariables)
}
