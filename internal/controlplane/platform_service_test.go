package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type staticCNAMEResolver map[string]string

func (r staticCNAMEResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if target, ok := r[name]; ok {
		return target, nil
	}
	return "", errors.New("not found")
}

func (r staticCNAMEResolver) LookupHost(context.Context, string) ([]string, error) {
	return nil, errors.New("not found")
}

type staticDNSResolver struct {
	cname map[string]string
	hosts map[string][]string
}

func (r staticDNSResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if target, ok := r.cname[name]; ok {
		return target, nil
	}
	return "", errors.New("not found")
}

func (r staticDNSResolver) LookupHost(_ context.Context, name string) ([]string, error) {
	if addrs, ok := r.hosts[name]; ok {
		return append([]string(nil), addrs...), nil
	}
	return nil, errors.New("not found")
}

func contextWithDelegatedUser(userID, _ string) context.Context {
	return identity.WithDelegatedUser(context.Background(), userID)
}

type noopNotifier struct{}

func (noopNotifier) Notify(agentID string) {}

type noopIngress struct{}

func (noopIngress) Sync(ctx context.Context) error { return nil }

func (noopIngress) RequestSync() {}

func (noopIngress) Converged(ctx context.Context) (bool, error) {
	return true, nil
}

type countingIngress struct {
	requests atomic.Int32
}

func (c *countingIngress) Sync(ctx context.Context) error { return nil }

func (c *countingIngress) Converged(ctx context.Context) (bool, error) {
	return true, nil
}

func (c *countingIngress) RequestSync() {
	c.requests.Add(1)
}

type fakePlatformDelivery struct {
	applyDeploymentActionFn  func(ctx context.Context, user authz.User, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.DeploymentActionResult, error)
	releaseEnvironmentFn     func(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ReleasedService, error)
	createScheduledServiceFn func(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error)
	updateServiceFn          func(ctx context.Context, user authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error)
	discardServiceChangesFn  func(ctx context.Context, user authz.User, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error)
	deleteServiceFn          func(ctx context.Context, user authz.User, serviceID string) error
	restoreServiceFn         func(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	scaleServiceFn           func(ctx context.Context, user authz.User, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error)
	liveAllocationsFn        func(environmentID string) (map[string][]deliverycore.AllocationRecord, error)
	duplicateEnvironmentFn   func(ctx context.Context, user authz.User, sourceEnvironmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error)
}

func (f *fakePlatformDelivery) LiveAllocationsByEnvironment(environmentID string) (map[string][]deliverycore.AllocationRecord, error) {
	if f.liveAllocationsFn != nil {
		return f.liveAllocationsFn(environmentID)
	}
	return map[string][]deliverycore.AllocationRecord{}, nil
}

func (f *fakePlatformDelivery) ReleaseEnvironment(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ReleasedService, error) {
	if f.releaseEnvironmentFn != nil {
		return f.releaseEnvironmentFn(ctx, user, environmentID)
	}
	return nil, nil
}

func (f *fakePlatformDelivery) ApplyDeploymentAction(ctx context.Context, user authz.User, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.DeploymentActionResult, error) {
	if f.applyDeploymentActionFn != nil {
		return f.applyDeploymentActionFn(ctx, user, serviceID, deploymentID, action, idempotencyKey, allocationID)
	}
	return deliverycore.DeploymentActionResult{}, nil
}

func (f *fakePlatformDelivery) CreateScheduledService(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
	if f.createScheduledServiceFn != nil {
		return f.createScheduledServiceFn(ctx, user, environmentID, name, spec)
	}
	return deliverycore.ServiceRecord{ID: "service-1", EnvironmentID: environmentID, Name: name, Spec: spec, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformDelivery) UpdateService(ctx context.Context, user authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
	if f.updateServiceFn != nil {
		return f.updateServiceFn(ctx, user, serviceID, name, spec)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1", Name: name, Spec: spec, AllocatedAgentID: "node-1"}, true, nil
}

func (f *fakePlatformDelivery) DiscardServiceChanges(ctx context.Context, user authz.User, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error) {
	if f.discardServiceChangesFn != nil {
		return f.discardServiceChangesFn(ctx, user, serviceID, changeIDs, discardAll)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1", AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformDelivery) DeleteService(ctx context.Context, user authz.User, serviceID string) error {
	if f.deleteServiceFn != nil {
		return f.deleteServiceFn(ctx, user, serviceID)
	}
	return nil
}

func (f *fakePlatformDelivery) RestoreService(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error) {
	if f.restoreServiceFn != nil {
		return f.restoreServiceFn(ctx, user, serviceID)
	}
	return deliverycore.ServiceRecord{ID: serviceID}, nil
}

func (f *fakePlatformDelivery) ScaleService(ctx context.Context, user authz.User, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error) {
	if f.scaleServiceFn != nil {
		return f.scaleServiceFn(ctx, user, serviceID, desired)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1"}, nil, 0, nil
}

func (f *fakePlatformDelivery) BuildAttempts(ctx context.Context, user authz.User, serviceID, buildID string) ([]deliverycore.BuildAttemptRecord, error) {
	return nil, nil
}

func (f *fakePlatformDelivery) ListServiceArtifacts(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.BuildArtifactRecord, error) {
	return nil, nil
}

func (f *fakePlatformDelivery) GrowVolume(ctx context.Context, user authz.User, volumeID string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	return deliverycore.VolumeRecord{ID: volumeID, SizeBytes: sizeBytes}, nil
}

func (f *fakePlatformDelivery) RenderVolumeStatus(rec deliverycore.VolumeRecord) deliverycore.VolumeRecord {
	return rec
}

func (f *fakePlatformDelivery) LivePosition() deliverycore.LivePosition {
	return deliverycore.LivePosition{}
}

type fakePlatformStore struct {
	createProjectFn                   func(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error)
	listProjectsFn                    func(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error)
	projectByIDFn                     func(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	updateProjectLogRetentionFn       func(ctx context.Context, user authz.User, projectID string, retentionDays int32) (deliverycore.ProjectRecord, error)
	serviceByIDFn                     func(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	listServicesFn                    func(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.ServiceRecord, error)
	createScheduledVolumeFn           func(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	listVolumesFn                     func(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.VolumeRecord, error)
	deleteVolumeFn                    func(ctx context.Context, user authz.User, volumeID, confirmation string) error
	createDomainBindingFn             func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	createPlatformDomainBindingFn     func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	platformDomainBindingForServiceFn func(ctx context.Context, user authz.User, serviceID string) (deliverycore.DomainBindingRecord, error)
	updateDomainBindingFn             func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	domainBindingByHostFn             func(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error)
	listDomainBindingsFn              func(ctx context.Context, user authz.User, serviceID string, includeDeleted bool) ([]deliverycore.DomainBindingRecord, error)
	deleteDomainBindingFn             func(ctx context.Context, user authz.User, hostname string) (bool, error)
	serviceStatusFn                   func(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error)
	listServiceDeploymentsFn          func(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error)
	listAllocationsByServiceIDFn      func(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error)
	listAgentsFn                      func(ctx context.Context, user authz.User) ([]deliverycore.AgentRecord, error)
	agentIDsFn                        func(context.Context) ([]string, error)
	listEnvironmentsFn                func(ctx context.Context, user authz.User, projectID string, includeDeleted bool) ([]deliverycore.EnvironmentRecord, error)
	environmentByIDFn                 func(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error)
	createEnvironmentFn               func(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error)
	renameEnvironmentFn               func(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error)
	updateEnvironmentAutoDeployFn     func(ctx context.Context, user authz.User, environmentID string, autoDeploy bool) (deliverycore.EnvironmentRecord, error)
	deleteEnvironmentFn               func(ctx context.Context, user authz.User, environmentID, confirmation string) ([]string, error)
	restoreEnvironmentFn              func(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error)
	deleteProjectFn                   func(ctx context.Context, user authz.User, projectID, confirmation string) ([]string, error)
	restoreProjectFn                  func(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	previewProjectDeletionFn          func(ctx context.Context, user authz.User, projectID string) (deletionPreview, error)
	previewEnvironmentDeletionFn      func(ctx context.Context, user authz.User, environmentID string) (deletionPreview, error)
	previewVolumeDeletionFn           func(ctx context.Context, user authz.User, volumeID string) (deletionPreview, error)
	restoreDomainBindingFn            func(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error)
}

func (f *fakePlatformStore) listEnvironments(ctx context.Context, user authz.User, projectID string, includeDeleted bool) ([]deliverycore.EnvironmentRecord, error) {
	if f.listEnvironmentsFn != nil {
		return f.listEnvironmentsFn(ctx, user, projectID, includeDeleted)
	}
	return nil, nil
}

func (f *fakePlatformStore) EnvironmentByID(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error) {
	if f.environmentByIDFn != nil {
		return f.environmentByIDFn(ctx, user, environmentID)
	}
	return deliverycore.EnvironmentRecord{ID: environmentID, ProjectID: "project-1", Kind: deliverycore.EnvironmentKindPersistent}, nil
}

func (f *fakePlatformStore) createEnvironment(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error) {
	if f.createEnvironmentFn != nil {
		return f.createEnvironmentFn(ctx, user, projectID, name)
	}
	return deliverycore.EnvironmentRecord{ID: "environment-1", ProjectID: projectID, Name: name, Kind: deliverycore.EnvironmentKindPersistent}, nil
}

func (f *fakePlatformDelivery) DuplicateEnvironment(ctx context.Context, user authz.User, sourceEnvironmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error) {
	if f.duplicateEnvironmentFn != nil {
		return f.duplicateEnvironmentFn(ctx, user, sourceEnvironmentID, name, copyVariables)
	}
	return deliverycore.EnvironmentRecord{ID: "environment-2", ProjectID: "project-1", Name: name, Kind: deliverycore.EnvironmentKindPersistent, CopiedFromEnvironmentID: sourceEnvironmentID}, nil
}

func (f *fakePlatformStore) renameEnvironment(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error) {
	if f.renameEnvironmentFn != nil {
		return f.renameEnvironmentFn(ctx, user, environmentID, name)
	}
	return deliverycore.EnvironmentRecord{ID: environmentID, ProjectID: "project-1", Name: name, Kind: deliverycore.EnvironmentKindPersistent}, nil
}

func (f *fakePlatformStore) updateEnvironmentAutoDeploy(ctx context.Context, user authz.User, environmentID string, autoDeploy bool) (deliverycore.EnvironmentRecord, error) {
	if f.updateEnvironmentAutoDeployFn != nil {
		return f.updateEnvironmentAutoDeployFn(ctx, user, environmentID, autoDeploy)
	}
	return deliverycore.EnvironmentRecord{ID: environmentID, ProjectID: "project-1", Kind: deliverycore.EnvironmentKindPersistent, AutoDeploy: autoDeploy}, nil
}

func (f *fakePlatformStore) deleteEnvironment(ctx context.Context, user authz.User, environmentID, confirmation string) ([]string, error) {
	if f.deleteEnvironmentFn != nil {
		return f.deleteEnvironmentFn(ctx, user, environmentID, confirmation)
	}
	return nil, nil
}

func (f *fakePlatformStore) restoreEnvironment(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error) {
	if f.restoreEnvironmentFn != nil {
		return f.restoreEnvironmentFn(ctx, user, environmentID)
	}
	return deliverycore.EnvironmentRecord{ID: environmentID}, nil
}

func (f *fakePlatformStore) createProject(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error) {
	if f.createProjectFn != nil {
		return f.createProjectFn(ctx, user, name)
	}
	return deliverycore.ProjectRecord{ID: "project-1", Name: name, Kind: deliverycore.ProjectKindUser, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakePlatformStore) listProjects(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error) {
	if f.listProjectsFn != nil {
		return f.listProjectsFn(ctx, user, includeDeleted)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteProject(ctx context.Context, user authz.User, projectID, confirmation string) ([]string, error) {
	if f.deleteProjectFn != nil {
		return f.deleteProjectFn(ctx, user, projectID, confirmation)
	}
	return nil, nil
}

func (f *fakePlatformStore) restoreProject(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error) {
	if f.restoreProjectFn != nil {
		return f.restoreProjectFn(ctx, user, projectID)
	}
	return deliverycore.ProjectRecord{ID: projectID}, nil
}

func (f *fakePlatformStore) previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (deletionPreview, error) {
	if f.previewProjectDeletionFn != nil {
		return f.previewProjectDeletionFn(ctx, user, projectID)
	}
	return deletionPreview{}, nil
}

func (f *fakePlatformStore) previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (deletionPreview, error) {
	if f.previewEnvironmentDeletionFn != nil {
		return f.previewEnvironmentDeletionFn(ctx, user, environmentID)
	}
	return deletionPreview{}, nil
}

func (f *fakePlatformStore) previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (deletionPreview, error) {
	if f.previewVolumeDeletionFn != nil {
		return f.previewVolumeDeletionFn(ctx, user, volumeID)
	}
	return deletionPreview{}, nil
}

func (f *fakePlatformStore) projectByID(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error) {
	if f.projectByIDFn != nil {
		return f.projectByIDFn(ctx, user, projectID)
	}
	return deliverycore.ProjectRecord{ID: projectID}, nil
}

func (f *fakePlatformStore) updateProjectLogRetention(ctx context.Context, user authz.User, projectID string, retentionDays int32) (deliverycore.ProjectRecord, error) {
	if f.updateProjectLogRetentionFn != nil {
		return f.updateProjectLogRetentionFn(ctx, user, projectID, retentionDays)
	}
	return deliverycore.ProjectRecord{ID: projectID, LogRetentionDays: retentionDays}, nil
}

func (f *fakePlatformStore) ServiceByID(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error) {
	if f.serviceByIDFn != nil {
		return f.serviceByIDFn(ctx, user, serviceID)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1", AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) ListServices(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.ServiceRecord, error) {
	if f.listServicesFn != nil {
		return f.listServicesFn(ctx, user, environmentID, includeDeleted)
	}
	return nil, nil
}

func (f *fakePlatformStore) createScheduledVolume(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	if f.createScheduledVolumeFn != nil {
		return f.createScheduledVolumeFn(ctx, user, environmentID, name, sizeBytes)
	}
	return deliverycore.VolumeRecord{ID: "volume-1", EnvironmentID: environmentID, Name: name, SizeBytes: sizeBytes}, nil
}

func (f *fakePlatformStore) listVolumes(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.VolumeRecord, error) {
	if f.listVolumesFn != nil {
		return f.listVolumesFn(ctx, user, environmentID, includeDeleted)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteVolume(ctx context.Context, user authz.User, volumeID, confirmation string) error {
	if f.deleteVolumeFn != nil {
		return f.deleteVolumeFn(ctx, user, volumeID, confirmation)
	}
	return sql.ErrNoRows
}

func (f *fakePlatformStore) CreateDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	if f.createDomainBindingFn != nil {
		return f.createDomainBindingFn(ctx, user, hostname, serviceID, targetPort)
	}
	return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: serviceID, TargetPort: targetPort}, true, nil
}

func (f *fakePlatformStore) CreatePlatformDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	if f.createPlatformDomainBindingFn != nil {
		return f.createPlatformDomainBindingFn(ctx, user, hostname, serviceID, targetPort)
	}
	return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: serviceID, TargetPort: targetPort, PlatformGenerated: true}, true, nil
}

func (f *fakePlatformStore) PlatformDomainBindingForService(ctx context.Context, user authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
	if f.platformDomainBindingForServiceFn != nil {
		return f.platformDomainBindingForServiceFn(ctx, user, serviceID)
	}
	return deliverycore.DomainBindingRecord{}, sql.ErrNoRows
}

func (f *fakePlatformStore) UpdateDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	if f.updateDomainBindingFn != nil {
		return f.updateDomainBindingFn(ctx, user, hostname, serviceID, targetPort)
	}
	return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: serviceID, TargetPort: targetPort}, false, nil
}

func (f *fakePlatformStore) DomainBindingByHostname(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error) {
	if f.domainBindingByHostFn != nil {
		return f.domainBindingByHostFn(ctx, user, hostname)
	}
	return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: "service-1"}, nil
}

func (f *fakePlatformStore) ListDomainBindings(ctx context.Context, user authz.User, serviceID string, includeDeleted bool) ([]deliverycore.DomainBindingRecord, error) {
	if f.listDomainBindingsFn != nil {
		return f.listDomainBindingsFn(ctx, user, serviceID, includeDeleted)
	}
	return nil, nil
}

func (f *fakePlatformStore) DeleteDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (bool, error) {
	if f.deleteDomainBindingFn != nil {
		return f.deleteDomainBindingFn(ctx, user, hostname)
	}
	return true, nil
}

func (f *fakePlatformStore) RestoreDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error) {
	if f.restoreDomainBindingFn != nil {
		return f.restoreDomainBindingFn(ctx, user, hostname)
	}
	return deliverycore.DomainBindingRecord{Hostname: hostname}, nil
}

func (f *fakePlatformStore) ServiceStatus(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
	if f.serviceStatusFn != nil {
		return f.serviceStatusFn(ctx, user, serviceID)
	}
	return deliverycore.ServiceRecord{}, nil, nil
}

func (f *fakePlatformStore) ListServiceDeployments(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error) {
	if f.listServiceDeploymentsFn != nil {
		return f.listServiceDeploymentsFn(ctx, user, serviceID, limit)
	}
	return nil, nil
}

func (f *fakePlatformStore) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error) {
	if f.listAllocationsByServiceIDFn != nil {
		return f.listAllocationsByServiceIDFn(ctx, serviceID)
	}
	return nil, nil
}

func (f *fakePlatformStore) ListAgents(ctx context.Context, user authz.User) ([]deliverycore.AgentRecord, error) {
	if f.listAgentsFn != nil {
		return f.listAgentsFn(ctx, user)
	}
	return []deliverycore.AgentRecord{}, nil
}

func (f *fakePlatformStore) AgentIDs(ctx context.Context) ([]string, error) {
	if f.agentIDsFn != nil {
		return f.agentIDsFn(ctx)
	}
	return nil, nil
}

func TestCustomerRuntimeContractOmitsPrivilegedHostAndMountControls(t *testing.T) {
	fields := (&platformv1.ServiceRuntime{}).ProtoReflect().Descriptor().Fields()
	forbidden := map[string]struct{}{
		"privileged": {}, "host_network": {}, "host_pid": {}, "sysctls": {},
		"devices": {}, "bind_mounts": {}, "mounts": {}, "sandbox_profile": {},
	}
	for index := 0; index < fields.Len(); index++ {
		name := string(fields.Get(index).Name())
		if _, exists := forbidden[name]; exists {
			t.Fatalf("customer runtime exposes forbidden field %q", name)
		}
	}
}

func TestPlatformServiceGetServiceStatusRereadsAfterWait(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	var reads atomic.Int32
	service := newPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
			applied := int64(1)
			healthy := false
			if reads.Add(1) > 1 {
				applied = 2
				healthy = true
			}
			return deliverycore.ServiceRecord{
				ID:               serviceID,
				ProjectID:        "project-1",
				AllocatedAgentID: "node-1",
				CreatedAt:        now.Add(-10 * time.Minute),
				Spec:             directImageServiceSpec("nginx:1.27", nil),
				LatestBuild: &platformv1.BuildStatus{
					BuildId: "build-1",
				},
			}, []deliverycore.AllocationRecord{{
				ID:                       "alloc-status",
				ServiceID:                serviceID,
				AgentID:                  "node-1",
				DesiredRolloutGeneration: 2,
				AppliedRolloutGeneration: applied,
				Healthy:                  healthy,
				UpdatedAt:                now,
			}}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil)

	resp, err := service.GetServiceStatus(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetServiceStatusRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if got := resp.GetAllocation().GetAppliedRolloutGeneration(); got != 2 {
		t.Fatalf("expected the post-wait snapshot, got applied generation %d", got)
	}
	if !resp.GetAllocation().GetHealthy() {
		t.Fatalf("expected the post-wait snapshot to report healthy")
	}
}

func TestListServicesDecoratesFromSingleLiveAllocationSnapshot(t *testing.T) {
	t.Parallel()

	var allocationReads atomic.Int32
	store := &fakePlatformStore{
		listServicesFn: func(context.Context, authz.User, string, bool) ([]deliverycore.ServiceRecord, error) {
			return []deliverycore.ServiceRecord{
				{ID: "service-a", EnvironmentID: "environment-1", Spec: directImageServiceSpec("nginx:1.27", nil)},
				{ID: "service-b", EnvironmentID: "environment-1", Spec: directImageServiceSpec("nginx:1.27", nil)},
			}, nil
		},
		listAllocationsByServiceIDFn: func(context.Context, string) ([]deliverycore.AllocationRecord, error) {
			allocationReads.Add(1)
			return nil, nil
		},
	}
	delivery := &fakePlatformDelivery{liveAllocationsFn: func(environmentID string) (map[string][]deliverycore.AllocationRecord, error) {
		if environmentID != "environment-1" {
			t.Fatalf("environment = %q", environmentID)
		}
		return map[string][]deliverycore.AllocationRecord{
			"service-a": {{
				Healthy: true, AllocationIPv4: "10.0.0.1", AllocationIPv6: "fd00::1",
				DesiredSpecRevision: 1, AppliedSpecRevision: 1,
				DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1,
			}},
		}, nil
	}}
	service := newPlatformService(store, noopNotifier{}, noopIngress{}, delivery)

	resp, err := service.ListServices(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ListServicesRequest{EnvironmentId: "environment-1"})
	if err != nil {
		t.Fatal(err)
	}
	if allocationReads.Load() != 0 {
		t.Fatalf("per-service allocation reads = %d, want 0", allocationReads.Load())
	}
	if len(resp.GetServices()) != 2 || resp.GetServices()[0].GetReadyReplicaCount() != 1 || resp.GetServices()[1].GetReadyReplicaCount() != 0 {
		t.Fatalf("decorated services = %#v", resp.GetServices())
	}
}

type staticLiveOwner struct {
	held bool
	addr string
	err  error
}

func (o staticLiveOwner) Lookup(context.Context) (bool, string, error) {
	return o.held, o.addr, o.err
}

func newPlatformServiceWithOwner(owner staticLiveOwner) *platformService {
	return newPlatformService(
		&fakePlatformStore{},
		noopNotifier{},
		noopIngress{},
		&fakePlatformDelivery{},
		withPlatformLiveOwner(owner),
	)
}

func TestNonOwnerPlatformRPCsRedirectToLiveOwner(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	service := newPlatformServiceWithOwner(staticLiveOwner{held: false, addr: "owner.example:9443"})
	want := deliverycore.LiveOwnerRedirectMessage("owner.example:9443")

	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"CreateProject", func(ctx context.Context) error {
			_, err := service.CreateProject(ctx, &platformv1.CreateProjectRequest{Name: "project"})
			return err
		}},
		{"LinkGitHubRepository", func(ctx context.Context) error {
			_, err := service.LinkGitHubRepository(ctx, &platformv1.LinkGitHubRepositoryRequest{ProjectId: "project-1", RepositorySelector: "owner/repo"})
			return err
		}},
		{"GetServiceStatus", func(ctx context.Context) error {
			_, err := service.GetServiceStatus(ctx, &platformv1.GetServiceStatusRequest{ServiceId: "service-1"})
			return err
		}},
		{"GetService", func(ctx context.Context) error {
			_, err := service.GetService(ctx, &platformv1.GetServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ListServices", func(ctx context.Context) error {
			_, err := service.ListServices(ctx, &platformv1.ListServicesRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"ListAgents", func(ctx context.Context) error {
			_, err := service.ListAgents(ctx, &emptypb.Empty{})
			return err
		}},
		{"CreateService", func(ctx context.Context) error {
			_, err := service.CreateService(ctx, &platformv1.CreateServiceRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"UpdateService", func(ctx context.Context) error {
			_, err := service.UpdateService(ctx, &platformv1.UpdateServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ApplyDeploymentAction", func(ctx context.Context) error {
			_, err := service.ApplyDeploymentAction(ctx, &platformv1.ApplyDeploymentActionRequest{
				ServiceId: "service-1", DeploymentId: "deployment-1", IdempotencyKey: "key-1", Action: platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART,
			})
			return err
		}},
		{"ScaleService", func(ctx context.Context) error {
			_, err := service.ScaleService(ctx, &platformv1.ScaleServiceRequest{ServiceId: "service-1", DesiredReplicaCount: 2})
			return err
		}},
		{"DiscardServiceChanges", func(ctx context.Context) error {
			_, err := service.DiscardServiceChanges(ctx, &platformv1.DiscardServiceChangesRequest{ServiceId: "service-1", DiscardAll: true})
			return err
		}},
		{"DeleteService", func(ctx context.Context) error {
			_, err := service.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ReleaseEnvironment", func(ctx context.Context) error {
			_, err := service.ReleaseEnvironment(ctx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"CreateVolume", func(ctx context.Context) error {
			_, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 1})
			return err
		}},
		{"DeleteVolume", func(ctx context.Context) error {
			_, err := service.DeleteVolume(ctx, &platformv1.DeleteVolumeRequest{VolumeId: "volume-1"})
			return err
		}},
		{"CreateDomainBinding", func(ctx context.Context) error {
			_, err := service.CreateDomainBinding(ctx, &platformv1.CreateDomainBindingRequest{})
			return err
		}},
		{"GenerateDomainBinding", func(ctx context.Context) error {
			_, err := service.GenerateDomainBinding(ctx, &platformv1.GenerateDomainBindingRequest{ServiceId: "service-1"})
			return err
		}},
		{"UpdateDomainBinding", func(ctx context.Context) error {
			_, err := service.UpdateDomainBinding(ctx, &platformv1.UpdateDomainBindingRequest{Hostname: "web.example.com"})
			return err
		}},
		{"DeleteDomainBinding", func(ctx context.Context) error {
			_, err := service.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: "web.example.com"})
			return err
		}},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(ctx)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("non-owner %s error = %v, want FailedPrecondition redirect", tc.name, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("non-owner %s error = %v, want redirect %q", tc.name, err, want)
			}
		})
	}
}

func TestListAgentsMapsErrNotLiveOwner(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	store := &fakePlatformStore{listAgentsFn: func(context.Context, authz.User) ([]deliverycore.AgentRecord, error) {
		return nil, deliverycore.ErrNotLiveOwner
	}}
	service := newPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, withPlatformLiveOwner(staticLiveOwner{held: true}))
	_, err := service.ListAgents(ctx, &emptypb.Empty{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("ListAgents = %v, want Unavailable while the lease holder is not serving", err)
	}
}

func TestServiceReadsMapErrNotLiveOwnerAfterInitialOwnerCheck(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")

	t.Run("create reload", func(t *testing.T) {
		store := &fakePlatformStore{serviceByIDFn: func(context.Context, authz.User, string) (deliverycore.ServiceRecord, error) {
			return deliverycore.ServiceRecord{}, deliverycore.ErrNotLiveOwner
		}}
		service := newPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, withPlatformLiveOwner(staticLiveOwner{held: true}))
		_, err := service.CreateService(ctx, &platformv1.CreateServiceRequest{
			EnvironmentId: "environment-1",
			Service: &platformv1.ServiceInput{
				Name: "web",
				Spec: directImageServiceSpec("busybox:1.36", nil),
			},
		})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("CreateService = %v, want Unavailable", err)
		}
	})

	t.Run("delete delivery", func(t *testing.T) {
		delivery := &fakePlatformDelivery{deleteServiceFn: func(context.Context, authz.User, string) error {
			return deliverycore.ErrNotLiveOwner
		}}
		service := newPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, delivery, withPlatformLiveOwner(staticLiveOwner{held: true}))
		_, err := service.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: "service-1"})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("DeleteService = %v, want Unavailable", err)
		}
	})
}

func TestCreateVolumeMapsValidationErrors(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	store := &fakePlatformStore{}
	service := newPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, withPlatformLiveOwner(staticLiveOwner{held: true}))

	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{Name: "data", SizeBytes: 64 << 20}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty environment: %v", err)
	}
	for _, size := range []int64{1, 32 << 20, 64<<20 + 1} {
		if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: size}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("size %d: %v", size, err)
		}
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, sql.ErrNoRows
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "missing", Name: "data", SizeBytes: 64 << 20}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown environment: %v", err)
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, deliverycore.ErrVolumeAlreadyExists
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 64 << 20}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate name: %v", err)
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, deliverycore.ErrInvalidVolume
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 64 << 20}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("store validation: %v", err)
	}
}

func TestLiveOwnerPlatformRPCsAreNotRedirected(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	service := newPlatformServiceWithOwner(staticLiveOwner{held: true, addr: "owner.example:9443"})
	if _, err := service.ListAgents(ctx, &emptypb.Empty{}); err != nil {
		t.Fatalf("owner ListAgents error = %v, want nil", err)
	}
}
