package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/source"
)

type ReleasedService struct {
	Service     ServiceRecord
	Allocations []AllocationRecord
}

func (d *Delivery) ReleaseEnvironment(ctx context.Context, user authz.User, environmentID string) ([]ReleasedService, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return nil, err
	}
	// Direct-image tags resolve outside the product transaction and scheduler lock: registry
	// calls must never hold product locks or stall other serialized mutations. Only selected
	// services resolve, and a spec racing the pre-read retries with a fresh map.
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
	// The scheduler lock serializes the mutation phase only; pre-reads run outside it.
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
		if err := commitStagedVolumesTx(ctx, tx, environment.ID); err != nil {
			return err
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
				if err := d.store.requireVolumeAttachableQuerier(ctx, tx, environment.ID, service.ID, volumeName); err != nil {
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

func (d *Delivery) releaseServiceRevisionTx(ctx context.Context, tx *sql.Tx, env authz.Environment, serviceID string, resolved map[string]resolvedDirectImage) (ServiceRecord, error) {

	current, err := d.store.serviceByIDInEnvironmentQuerier(ctx, tx, env, serviceID)
	if err != nil {
		return ServiceRecord{}, err
	}
	// The release enumeration locks live services only; lock and re-check so backfills and
	// concurrent deletes cannot slip a release past a tombstone.
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
