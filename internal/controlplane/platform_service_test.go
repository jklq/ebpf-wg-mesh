package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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
	return context.WithValue(context.Background(), delegatedUserContextKey{}, DelegatedUser{
		UserID: userID,
	})
}

type noopNotifier struct{}

func (noopNotifier) Notify(agentID string) {}

type noopIngress struct{}

func (noopIngress) Sync(ctx context.Context) error { return nil }
func (noopIngress) RequestSync()                   {}

type countingIngress struct {
	requests atomic.Int32
}

func (c *countingIngress) Sync(ctx context.Context) error { return nil }

func (c *countingIngress) RequestSync() {
	c.requests.Add(1)
}

type fakePlatformStore struct {
	createProjectFn                   func(ctx context.Context, userID, name string) (projectRecord, error)
	listProjectsFn                    func(ctx context.Context, userID string) ([]projectRecord, error)
	projectByIDFn                     func(ctx context.Context, userID, projectID string) (projectRecord, error)
	authorizeProjectWriteFn           func(ctx context.Context, userID, projectID string) error
	createScheduledServiceFn          func(ctx context.Context, userID, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error)
	updateServiceFn                   func(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error)
	redeployServiceFn                 func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
	restartServiceFn                  func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
	applyDeploymentActionFn           func(ctx context.Context, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (serviceRecord, deploymentActionRecord, error)
	discardServiceChangesFn           func(ctx context.Context, userID, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error)
	requestServiceSourceSyncFn        func(ctx context.Context, userID, projectID, serviceID string) error
	enqueueBuildForServiceFn          func(ctx context.Context, userID, projectID, serviceID, commitSHA string) (buildRunRecord, error)
	deleteServiceFn                   func(ctx context.Context, userID, projectID, serviceID string) error
	serviceByIDFn                     func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
	listServicesFn                    func(ctx context.Context, userID, projectID string) ([]serviceRecord, error)
	createScheduledVolumeFn           func(ctx context.Context, userID, projectID, name string, sizeBytes int64) (volumeRecord, error)
	listVolumesFn                     func(ctx context.Context, userID, projectID string) ([]volumeRecord, error)
	deleteVolumeFn                    func(ctx context.Context, userID, projectID, volumeID string) error
	createDomainBindingFn             func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	createPlatformDomainBindingFn     func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	platformDomainBindingForServiceFn func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error)
	updateDomainBindingFn             func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	domainBindingByHostFn             func(ctx context.Context, userID, projectID, hostname string) (domainBindingRecord, error)
	listDomainBindingsFn              func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error)
	deleteDomainBindingFn             func(ctx context.Context, userID, projectID, hostname string) (bool, error)
	serviceStatusFn                   func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, []allocationRecord, error)
	scaleServiceFn                    func(ctx context.Context, userID, projectID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error)
	listServiceDeploymentsFn          func(ctx context.Context, userID, projectID, serviceID string, limit int32) ([]deploymentRecord, error)
	allocationByServiceIDFn           func(ctx context.Context, serviceID string) (allocationRecord, error)
	listAllocationsByServiceIDFn      func(ctx context.Context, serviceID string) ([]allocationRecord, error)
	listAgentsFn                      func(ctx context.Context) ([]agentRecord, error)
}

func (f *fakePlatformStore) listEnvironments(context.Context, string, string) ([]environmentRecord, error) {
	return nil, nil
}

func (f *fakePlatformStore) environmentByID(_ context.Context, _ string, environmentID string) (environmentRecord, error) {
	return environmentRecord{ID: environmentID, ProjectID: "project-1", Kind: environmentKindPersistent}, nil
}

func (f *fakePlatformStore) createEnvironment(_ context.Context, _ string, projectID, name string) (environmentRecord, error) {
	return environmentRecord{ID: "environment-1", ProjectID: projectID, Name: name, Kind: environmentKindPersistent}, nil
}

func (f *fakePlatformStore) duplicateEnvironment(_ context.Context, _ string, sourceEnvironmentID, name string, _ bool) (environmentRecord, error) {
	return environmentRecord{ID: "environment-2", ProjectID: "project-1", Name: name, Kind: environmentKindPersistent, CopiedFromEnvironmentID: sourceEnvironmentID}, nil
}

func (f *fakePlatformStore) renameEnvironment(_ context.Context, _ string, environmentID, name string) (environmentRecord, error) {
	return environmentRecord{ID: environmentID, ProjectID: "project-1", Name: name, Kind: environmentKindPersistent}, nil
}

func (f *fakePlatformStore) deleteEnvironment(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (f *fakePlatformStore) deployEnvironment(context.Context, string, string) ([]serviceRecord, []string, error) {
	return nil, nil, nil
}

func (f *fakePlatformStore) createProject(ctx context.Context, userID, name string) (projectRecord, error) {
	if f.createProjectFn != nil {
		return f.createProjectFn(ctx, userID, name)
	}
	return projectRecord{ID: "project-1", Name: name, Kind: projectKindUser, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakePlatformStore) listProjects(ctx context.Context, userID string) ([]projectRecord, error) {
	if f.listProjectsFn != nil {
		return f.listProjectsFn(ctx, userID)
	}
	return nil, nil
}

func (f *fakePlatformStore) projectByID(ctx context.Context, userID, projectID string) (projectRecord, error) {
	if f.projectByIDFn != nil {
		return f.projectByIDFn(ctx, userID, projectID)
	}
	return projectRecord{ID: projectID}, nil
}

func (f *fakePlatformStore) authorizeProjectWrite(ctx context.Context, userID, projectID string) error {
	if f.authorizeProjectWriteFn != nil {
		return f.authorizeProjectWriteFn(ctx, userID, projectID)
	}
	return nil
}

func (f *fakePlatformStore) createScheduledService(ctx context.Context, userID, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	if f.createScheduledServiceFn != nil {
		return f.createScheduledServiceFn(ctx, userID, projectID, name, spec)
	}
	return serviceRecord{ID: "service-1", EnvironmentID: projectID, Name: name, Spec: spec, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) updateService(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	if f.updateServiceFn != nil {
		return f.updateServiceFn(ctx, userID, projectID, serviceID, name, spec)
	}
	return serviceRecord{}, false, nil
}

func (f *fakePlatformStore) redeployService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	if f.redeployServiceFn != nil {
		return f.redeployServiceFn(ctx, userID, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) restartService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	if f.restartServiceFn != nil {
		return f.restartServiceFn(ctx, userID, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) applyDeploymentAction(ctx context.Context, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (serviceRecord, deploymentActionRecord, error) {
	if f.applyDeploymentActionFn != nil {
		return f.applyDeploymentActionFn(ctx, userID, serviceID, deploymentID, action, idempotencyKey, allocationID)
	}
	return serviceRecord{ID: serviceID, ProjectID: "project-1", EnvironmentID: "environment-1", AllocatedAgentID: "node-1"}, deploymentActionRecord{ID: "action-1", Action: deploymentActionName(action), TargetDeploymentID: deploymentID, IdempotencyKey: idempotencyKey, AllocationID: allocationID}, nil
}

func (f *fakePlatformStore) discardServiceChanges(ctx context.Context, userID, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error) {
	if f.discardServiceChangesFn != nil {
		return f.discardServiceChangesFn(ctx, userID, projectID, serviceID, changeIDs, discardAll)
	}
	return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) requestServiceSourceSync(ctx context.Context, userID, projectID, serviceID string) error {
	if f.requestServiceSourceSyncFn != nil {
		return f.requestServiceSourceSyncFn(ctx, userID, projectID, serviceID)
	}
	return nil
}

func (f *fakePlatformStore) enqueueBuildForService(ctx context.Context, userID, projectID, serviceID, commitSHA string) (buildRunRecord, error) {
	if f.enqueueBuildForServiceFn != nil {
		return f.enqueueBuildForServiceFn(ctx, userID, projectID, serviceID, commitSHA)
	}
	return buildRunRecord{ID: "build-1", ServiceID: serviceID, EnvironmentID: projectID, CommitSHA: commitSHA, State: buildStateQueued}, nil
}

func (f *fakePlatformStore) deleteService(ctx context.Context, userID, projectID, serviceID string) error {
	if f.deleteServiceFn != nil {
		return f.deleteServiceFn(ctx, userID, projectID, serviceID)
	}
	return nil
}

func (f *fakePlatformStore) serviceByID(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	if f.serviceByIDFn != nil {
		return f.serviceByIDFn(ctx, userID, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) listServices(ctx context.Context, userID, projectID string) ([]serviceRecord, error) {
	if f.listServicesFn != nil {
		return f.listServicesFn(ctx, userID, projectID)
	}
	return nil, nil
}

func (f *fakePlatformStore) createScheduledVolume(ctx context.Context, userID, projectID, name string, sizeBytes int64) (volumeRecord, error) {
	if f.createScheduledVolumeFn != nil {
		return f.createScheduledVolumeFn(ctx, userID, projectID, name, sizeBytes)
	}
	return volumeRecord{ID: "volume-1", EnvironmentID: projectID, Name: name, SizeBytes: sizeBytes}, nil
}

func (f *fakePlatformStore) listVolumes(ctx context.Context, userID, projectID string) ([]volumeRecord, error) {
	if f.listVolumesFn != nil {
		return f.listVolumesFn(ctx, userID, projectID)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteVolume(ctx context.Context, userID, projectID, volumeID string) error {
	if f.deleteVolumeFn != nil {
		return f.deleteVolumeFn(ctx, userID, projectID, volumeID)
	}
	return sql.ErrNoRows
}

func (f *fakePlatformStore) createDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if f.createDomainBindingFn != nil {
		return f.createDomainBindingFn(ctx, userID, projectID, hostname, serviceID, targetPort)
	}
	return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort}, true, nil
}

func (f *fakePlatformStore) createPlatformDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if f.createPlatformDomainBindingFn != nil {
		return f.createPlatformDomainBindingFn(ctx, userID, projectID, hostname, serviceID, targetPort)
	}
	return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort, PlatformGenerated: true}, true, nil
}

func (f *fakePlatformStore) platformDomainBindingForService(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
	if f.platformDomainBindingForServiceFn != nil {
		return f.platformDomainBindingForServiceFn(ctx, userID, projectID, serviceID)
	}
	return domainBindingRecord{}, sql.ErrNoRows
}

func (f *fakePlatformStore) updateDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if f.updateDomainBindingFn != nil {
		return f.updateDomainBindingFn(ctx, userID, projectID, hostname, serviceID, targetPort)
	}
	return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort}, false, nil
}

func (f *fakePlatformStore) domainBindingByHostname(ctx context.Context, userID, projectID, hostname string) (domainBindingRecord, error) {
	if f.domainBindingByHostFn != nil {
		return f.domainBindingByHostFn(ctx, userID, projectID, hostname)
	}
	return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: "service-1"}, nil
}

func (f *fakePlatformStore) listDomainBindings(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
	if f.listDomainBindingsFn != nil {
		return f.listDomainBindingsFn(ctx, userID, projectID, serviceID)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteDomainBinding(ctx context.Context, userID, projectID, hostname string) (bool, error) {
	if f.deleteDomainBindingFn != nil {
		return f.deleteDomainBindingFn(ctx, userID, projectID, hostname)
	}
	return true, nil
}

func (f *fakePlatformStore) serviceStatus(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, []allocationRecord, error) {
	if f.serviceStatusFn != nil {
		return f.serviceStatusFn(ctx, userID, projectID, serviceID)
	}
	return serviceRecord{}, nil, nil
}

func (f *fakePlatformStore) scaleService(ctx context.Context, userID, projectID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error) {
	if f.scaleServiceFn != nil {
		return f.scaleServiceFn(ctx, userID, projectID, serviceID, desired)
	}
	return serviceRecord{}, nil, nil
}

func (f *fakePlatformStore) listServiceDeployments(ctx context.Context, userID, projectID, serviceID string, limit int32) ([]deploymentRecord, error) {
	if f.listServiceDeploymentsFn != nil {
		return f.listServiceDeploymentsFn(ctx, userID, projectID, serviceID, limit)
	}
	return nil, nil
}

func (f *fakePlatformStore) allocationByServiceID(ctx context.Context, serviceID string) (allocationRecord, error) {
	if f.allocationByServiceIDFn != nil {
		return f.allocationByServiceIDFn(ctx, serviceID)
	}
	return allocationRecord{}, nil
}

func (f *fakePlatformStore) listAllocationsByServiceID(ctx context.Context, serviceID string) ([]allocationRecord, error) {
	if f.listAllocationsByServiceIDFn != nil {
		return f.listAllocationsByServiceIDFn(ctx, serviceID)
	}
	if f.allocationByServiceIDFn != nil {
		alloc, err := f.allocationByServiceIDFn(ctx, serviceID)
		if err != nil || alloc.ID == "" {
			return nil, err
		}
		return []allocationRecord{alloc}, nil
	}
	return nil, nil
}

func (f *fakePlatformStore) listAgents(ctx context.Context) ([]agentRecord, error) {
	if f.listAgentsFn != nil {
		return f.listAgentsFn(ctx)
	}
	return []agentRecord{}, nil
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
