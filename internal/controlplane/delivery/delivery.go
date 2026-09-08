package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"
)

// Delivery owns deployment lifecycle policy, its transactional state changes,
// and the wake, ingress, and publication effects that follow a commit. Its
// persistence and transaction helpers are private to this package.
type Delivery struct {
	store           *persistence
	notifier        PlatformNotifier
	ingress         PlatformIngress
	events          Events
	logEmitter      *logs.LogEmitter
	userFromContext func(context.Context) (UserIdentity, error)
	rolloutNow      func() time.Time
	failoverNow     func() time.Time
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

// ApplyDeploymentAction applies restart, rollback, cancellation, removal, and
// retry policy atomically, then performs every wake and publication required to
// make the committed state observable. Callers do not need to finish the
// operation themselves.
func (d *Delivery) ApplyDeploymentAction(ctx context.Context, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (DeploymentActionResult, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return DeploymentActionResult{}, err
	}
	service, _, agentIDs, err := d.applyDeploymentAction(ctx, identity.UserID, serviceID, deploymentID, action, idempotencyKey, allocationID)
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
	service, allocations, err := d.store.serviceStatus(ctx, identity.UserID, serviceID)
	if err != nil {
		return DeploymentActionResult{}, err
	}
	var eventIndex int64
	if d.events != nil {
		eventIndex, err = d.events.Publish(ctx, service.EnvironmentID)
		if err != nil {
			return DeploymentActionResult{}, err
		}
	}
	return DeploymentActionResult{Service: service, Allocations: allocations, EventIndex: eventIndex}, nil
}

// ReleaseEnvironment authorizes the delegated user and atomically releases all
// pending drafts, queues source work, advances rollouts and desired revisions,
// and snapshots the result. The transaction also advances the durable platform
// event index. Agent notifications are wake hints sent only after commit.
func (d *Delivery) ReleaseEnvironment(ctx context.Context, environmentID string) ([]ReleasedService, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return nil, err
	}
	userID := identity.UserID
	var services []ReleasedService
	var (
		serviceIDs             []string
		agentIDs               []string
		identityCatalogChanged bool
	)
	err = d.store.withTx(ctx, func(tx *sql.Tx) error {
		services = nil
		serviceIDs = nil
		agentIDs = nil
		identityCatalogChanged = false
		environment, err := d.store.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, serviceID := range serviceIDs {
			service, err := d.store.serviceByIDQuerier(ctx, tx, userID, serviceID)
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
			needsSourceBuild, err := d.serviceNeedsSourceBuildTx(ctx, tx, service)
			if err != nil {
				return err
			}
			released, err := d.releaseServiceRevisionTx(ctx, tx, userID, serviceID)
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
			if err := dbtx.BumpAllDesiredRevisions(ctx, tx); err != nil {
				return err
			}
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
		} else if err := dbtx.BumpDesiredRevisions(ctx, tx, agentIDs); err != nil {
			return err
		}
		for _, id := range serviceIDs {
			service, err := d.store.serviceByIDQuerier(ctx, tx, userID, id)
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
		return nil, fmt.Errorf("release environment: %w", err)
	}
	for _, agentID := range agentIDs {
		if d.notifier != nil {
			d.notifier.Notify(agentID)
		}
	}
	return services, nil
}

// serviceNeedsSourceBuildTx distinguishes source changes from runtime-only
// changes. A source-backed service needs a build for its first rollout and when
// its repository/ref/build recipe changes. Runtime, restart, and replica-only
// revisions reuse the image already resolved by the deployed rollout.
func (d *Delivery) serviceNeedsSourceBuildTx(ctx context.Context, tx *sql.Tx, service ServiceRecord) (bool, error) {
	if source.DesiredSourceSpec(service.Spec) == nil {
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
	deployedSpec, err := d.store.loadServiceDetailsQuerier(ctx, tx, service.ID, deployedSpecRevision)
	if err != nil {
		return false, err
	}
	return !sameDesiredSourceSpec(deployedSpec, service.Spec), nil
}

func (d *Delivery) releaseServiceRevisionTx(ctx context.Context, tx *sql.Tx, userID, serviceID string) (ServiceRecord, error) {
	current, err := d.store.serviceByIDQuerier(ctx, tx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if _, err := d.store.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
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
	existing, err := d.store.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, true)
	if err != nil {
		return ServiceRecord{}, err
	}
	now := time.Now().UTC()
	if _, err := d.prepareReplacementRolloutTx(ctx, tx, current, existing, now); err != nil {
		return ServiceRecord{}, err
	}
	nextRolloutGeneration := current.RolloutGeneration + 1
	resolvedImage := current.ResolvedImage
	if directImage := directImageRef(current.Spec); directImage != "" {
		resolvedImage = directImage
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET current_rollout_generation = $1,
		        current_resolved_image = $2,
		        desired_replica_count = $3,
		        updated_at = $4
		  WHERE id = $5
		    AND current_spec_revision = $6
		    AND current_rollout_generation = $7`,
		nextRolloutGeneration, resolvedImage, current.DesiredReplicaCount, now, serviceID, current.SpecRevision, current.RolloutGeneration,
	)
	if err != nil {
		return ServiceRecord{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ServiceRecord{}, err
	}
	if affected == 0 {
		return ServiceRecord{}, ErrConcurrentUpdate
	}
	if err := d.store.insertServiceRolloutTx(ctx, tx, serviceID, nextRolloutGeneration, current.SpecRevision, "environment_release", "", userID, now); err != nil {
		return ServiceRecord{}, err
	}
	releaseState := DeploymentStateScheduling
	releaseDetail := "Environment release scheduled"
	if source.DesiredSourceSpec(current.Spec) != nil && resolvedImage == "" {
		releaseState = DeploymentStateStaged
		releaseDetail = "Environment release waiting for source build"
	}
	dep, err := d.store.insertDeploymentTx(ctx, tx, serviceID, releaseState, deploymentActor{Kind: DeploymentCauseUser, ID: userID}, reasonEnvironmentRelease, releaseDetail, current.SpecRevision, nextRolloutGeneration, "", resolvedImage, userID, now)
	if err != nil {
		return ServiceRecord{}, err
	}
	current.LatestDeployment = &dep
	current.RolloutGeneration = nextRolloutGeneration
	current.ResolvedImage = resolvedImage
	current.PendingChanges = false
	current.UpdatedAt = now
	if _, err := d.advanceRolloutTx(ctx, tx, serviceID, now); err != nil {
		return ServiceRecord{}, err
	}
	return current, nil
}

// DuplicateEnvironment copies drafts, volumes, and staged deployments atomically.
func (r *Delivery) DuplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (EnvironmentRecord, error) {
	return r.store.duplicateEnvironment(ctx, userID, sourceEnvironmentID, name, copyVariables)
}
