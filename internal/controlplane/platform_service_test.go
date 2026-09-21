package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
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

type countingIngress struct {
	requests atomic.Int32
}

func (c *countingIngress) Sync(ctx context.Context) error { return nil }

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

func (f *fakePlatformDelivery) ScaleService(ctx context.Context, user authz.User, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error) {
	if f.scaleServiceFn != nil {
		return f.scaleServiceFn(ctx, user, serviceID, desired)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1"}, nil, 0, nil
}

func (f *fakePlatformDelivery) SealServiceSecret(ctx context.Context, user authz.User, serviceID, name string, value []byte) (int64, error) {
	return 1, nil
}

func (f *fakePlatformDelivery) DeleteServiceSecret(ctx context.Context, user authz.User, serviceID, name string) error {
	return nil
}

func (f *fakePlatformDelivery) ListServiceSecrets(ctx context.Context, user authz.User, serviceID string) ([]secretkeys.SecretMetadata, error) {
	return nil, nil
}

func (f *fakePlatformDelivery) LivePosition() deliverycore.LivePosition {
	return deliverycore.LivePosition{}
}

type fakePlatformStore struct {
	createProjectFn                   func(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error)
	listProjectsFn                    func(ctx context.Context, user authz.User) ([]deliverycore.ProjectRecord, error)
	projectByIDFn                     func(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	serviceByIDFn                     func(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	listServicesFn                    func(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ServiceRecord, error)
	createScheduledVolumeFn           func(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	listVolumesFn                     func(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.VolumeRecord, error)
	deleteVolumeFn                    func(ctx context.Context, user authz.User, volumeID string) error
	createDomainBindingFn             func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	createPlatformDomainBindingFn     func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	platformDomainBindingForServiceFn func(ctx context.Context, user authz.User, serviceID string) (deliverycore.DomainBindingRecord, error)
	updateDomainBindingFn             func(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error)
	domainBindingByHostFn             func(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error)
	listDomainBindingsFn              func(ctx context.Context, user authz.User, serviceID string) ([]deliverycore.DomainBindingRecord, error)
	deleteDomainBindingFn             func(ctx context.Context, user authz.User, hostname string) (bool, error)
	serviceStatusFn                   func(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error)
	listServiceDeploymentsFn          func(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error)
	listAllocationsByServiceIDFn      func(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error)
	listAgentsFn                      func(ctx context.Context, user authz.User) ([]deliverycore.AgentRecord, error)
	agentIDsFn                        func(context.Context) ([]string, error)
	listEnvironmentsFn                func(ctx context.Context, user authz.User, projectID string) ([]deliverycore.EnvironmentRecord, error)
	environmentByIDFn                 func(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error)
	createEnvironmentFn               func(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error)
	renameEnvironmentFn               func(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error)
	updateEnvironmentAutoDeployFn     func(ctx context.Context, user authz.User, environmentID string, autoDeploy bool) (deliverycore.EnvironmentRecord, error)
	deleteEnvironmentFn               func(ctx context.Context, user authz.User, environmentID string) ([]string, error)
}

func (f *fakePlatformStore) listEnvironments(ctx context.Context, user authz.User, projectID string) ([]deliverycore.EnvironmentRecord, error) {
	if f.listEnvironmentsFn != nil {
		return f.listEnvironmentsFn(ctx, user, projectID)
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

func (f *fakePlatformStore) deleteEnvironment(ctx context.Context, user authz.User, environmentID string) ([]string, error) {
	if f.deleteEnvironmentFn != nil {
		return f.deleteEnvironmentFn(ctx, user, environmentID)
	}
	return nil, nil
}

func (f *fakePlatformStore) createProject(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error) {
	if f.createProjectFn != nil {
		return f.createProjectFn(ctx, user, name)
	}
	return deliverycore.ProjectRecord{ID: "project-1", Name: name, Kind: deliverycore.ProjectKindUser, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakePlatformStore) listProjects(ctx context.Context, user authz.User) ([]deliverycore.ProjectRecord, error) {
	if f.listProjectsFn != nil {
		return f.listProjectsFn(ctx, user)
	}
	return nil, nil
}

func (f *fakePlatformStore) projectByID(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error) {
	if f.projectByIDFn != nil {
		return f.projectByIDFn(ctx, user, projectID)
	}
	return deliverycore.ProjectRecord{ID: projectID}, nil
}

func (f *fakePlatformStore) ServiceByID(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error) {
	if f.serviceByIDFn != nil {
		return f.serviceByIDFn(ctx, user, serviceID)
	}
	return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1", AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) ListServices(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ServiceRecord, error) {
	if f.listServicesFn != nil {
		return f.listServicesFn(ctx, user, environmentID)
	}
	return nil, nil
}

func (f *fakePlatformStore) createScheduledVolume(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	if f.createScheduledVolumeFn != nil {
		return f.createScheduledVolumeFn(ctx, user, environmentID, name, sizeBytes)
	}
	return deliverycore.VolumeRecord{ID: "volume-1", EnvironmentID: environmentID, Name: name, SizeBytes: sizeBytes}, nil
}

func (f *fakePlatformStore) listVolumes(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.VolumeRecord, error) {
	if f.listVolumesFn != nil {
		return f.listVolumesFn(ctx, user, environmentID)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteVolume(ctx context.Context, user authz.User, volumeID string) error {
	if f.deleteVolumeFn != nil {
		return f.deleteVolumeFn(ctx, user, volumeID)
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

func (f *fakePlatformStore) ListDomainBindings(ctx context.Context, user authz.User, serviceID string) ([]deliverycore.DomainBindingRecord, error) {
	if f.listDomainBindingsFn != nil {
		return f.listDomainBindingsFn(ctx, user, serviceID)
	}
	return nil, nil
}

func (f *fakePlatformStore) DeleteDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (bool, error) {
	if f.deleteDomainBindingFn != nil {
		return f.deleteDomainBindingFn(ctx, user, hostname)
	}
	return true, nil
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
