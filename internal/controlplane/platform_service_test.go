package controlplane

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestPlatformServiceEnsurePrincipalValidatesInput(t *testing.T) {
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{})

	_, err := service.EnsurePrincipal(context.Background(), &platformv1.EnsurePrincipalRequest{
		Subject: "",
		Email:   "user@example.com",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestPlatformServiceListProjectsUsesDelegatedUser(t *testing.T) {
	store := &fakePlatformStore{
		listProjectsFn: func(ctx context.Context, subject string) ([]projectRecord, error) {
			if subject != "user-1" {
				t.Fatalf("unexpected subject %q", subject)
			}
			return []projectRecord{{
				ID:        "project-1",
				Name:      "demo",
				Kind:      projectKindUser,
				CreatedAt: time.Now().UTC(),
			}}, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	resp, err := service.ListProjects(contextWithDelegatedUser("user-1", "user@example.com"), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(resp.GetProjects()) != 1 || resp.GetProjects()[0].GetId() != "project-1" {
		t.Fatalf("unexpected projects %+v", resp.GetProjects())
	}
}

func TestPlatformServiceCreateServiceMapsPlacementErrors(t *testing.T) {
	store := &fakePlatformStore{
		createScheduledServiceFn: func(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
			return serviceRecord{}, errNoPlacementAvailable
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		ProjectId: "project-1",
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: &platformv1.ServiceSpec{Image: "nginx:1.27"},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestPlatformServiceUpdateServiceMapsConcurrentUpdate(t *testing.T) {
	store := &fakePlatformStore{
		updateServiceFn: func(ctx context.Context, subject, projectID, serviceID string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{}, false, errConcurrentUpdate
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ProjectId: "project-1",
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: &platformv1.ServiceSpec{Image: "nginx:1.27"},
		},
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("expected Aborted, got %v", err)
	}
}

func TestPlatformServiceDeleteVolumeReturnsNotFound(t *testing.T) {
	service := NewPlatformService(&fakePlatformStore{
		listVolumesFn: func(ctx context.Context, subject, projectID string) ([]volumeRecord, error) {
			return []volumeRecord{}, nil
		},
	}, noopNotifier{}, noopIngress{})

	_, err := service.DeleteVolume(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteVolumeRequest{
		ProjectId: "project-1",
		VolumeId:  "missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPlatformServiceGetProjectMapsMissingProject(t *testing.T) {
	service := NewPlatformService(&fakePlatformStore{
		projectByIDFn: func(ctx context.Context, subject, projectID string) (projectRecord, error) {
			return projectRecord{}, sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{})

	_, err := service.GetProject(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetProjectRequest{
		ProjectId: "missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPlatformServiceUpdateServiceSkipsIngressRequest(t *testing.T) {
	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateServiceFn: func(ctx context.Context, subject, projectID, serviceID string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ProjectId: "project-1",
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: &platformv1.ServiceSpec{Image: "nginx:1.27"},
		},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if got := ingress.requests.Load(); got != 0 {
		t.Fatalf("expected no ingress requests, got %d", got)
	}
}

func TestPlatformServiceDeleteServiceRequestsIngressWhenServiceHasDomains(t *testing.T) {
	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error) {
			return []domainBindingRecord{{Hostname: "web.example.com", ProjectID: projectID, ServiceID: serviceID}}, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
		ProjectId: "project-1",
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceDeleteServiceSkipsIngressWhenServiceHasNoDomains(t *testing.T) {
	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error) {
			return nil, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
		ProjectId: "project-1",
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got := ingress.requests.Load(); got != 0 {
		t.Fatalf("expected no ingress requests, got %d", got)
	}
}

func TestPlatformServiceUpdateDomainBindingRequestsIngress(t *testing.T) {
	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateDomainBindingFn: func(ctx context.Context, subject, projectID, hostname, serviceID string) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateDomainBindingRequest{
		ProjectId: "project-1",
		Hostname:  "web.example.com",
		Binding:   &platformv1.DomainBindingTarget{ServiceId: "service-1"},
	})
	if err != nil {
		t.Fatalf("UpdateDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceDeleteDomainBindingRequestsIngress(t *testing.T) {
	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		deleteDomainBindingFn: func(ctx context.Context, subject, projectID, hostname string) (bool, error) {
			return true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteDomainBindingRequest{
		ProjectId: "project-1",
		Hostname:  "web.example.com",
	})
	if err != nil {
		t.Fatalf("DeleteDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func contextWithDelegatedUser(subject, email string) context.Context {
	return context.WithValue(context.Background(), delegatedUserContextKey{}, DelegatedUser{
		Subject: subject,
		Email:   email,
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
	ensurePrincipalFn        func(ctx context.Context, subject, email string) (userRecord, error)
	createProjectFn          func(ctx context.Context, subject, name string) (projectRecord, error)
	listProjectsFn           func(ctx context.Context, subject string) ([]projectRecord, error)
	projectByIDFn            func(ctx context.Context, subject, projectID string) (projectRecord, error)
	createScheduledServiceFn func(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error)
	updateServiceFn          func(ctx context.Context, subject, projectID, serviceID string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error)
	redeployServiceFn        func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	deleteServiceFn          func(ctx context.Context, subject, projectID, serviceID string) error
	serviceByIDFn            func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	listServicesFn           func(ctx context.Context, subject, projectID string) ([]serviceRecord, error)
	createScheduledVolumeFn  func(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error)
	listVolumesFn            func(ctx context.Context, subject, projectID string) ([]volumeRecord, error)
	deleteVolumeFn           func(ctx context.Context, subject, projectID, volumeID string) error
	createDomainBindingFn    func(ctx context.Context, subject, projectID, hostname, serviceID string) (domainBindingRecord, bool, error)
	updateDomainBindingFn    func(ctx context.Context, subject, projectID, hostname, serviceID string) (domainBindingRecord, bool, error)
	domainBindingByHostFn    func(ctx context.Context, subject, projectID, hostname string) (domainBindingRecord, error)
	listDomainBindingsFn     func(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error)
	deleteDomainBindingFn    func(ctx context.Context, subject, projectID, hostname string) (bool, error)
	serviceStatusFn          func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error)
	listAgentsFn             func(ctx context.Context) ([]agentRecord, error)
}

func (f *fakePlatformStore) ensurePrincipal(ctx context.Context, subject, email string) (userRecord, error) {
	if f.ensurePrincipalFn != nil {
		return f.ensurePrincipalFn(ctx, subject, email)
	}
	return userRecord{Subject: subject, Email: email, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakePlatformStore) createProject(ctx context.Context, subject, name string) (projectRecord, error) {
	if f.createProjectFn != nil {
		return f.createProjectFn(ctx, subject, name)
	}
	return projectRecord{ID: "project-1", Name: name, Kind: projectKindUser, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakePlatformStore) listProjects(ctx context.Context, subject string) ([]projectRecord, error) {
	if f.listProjectsFn != nil {
		return f.listProjectsFn(ctx, subject)
	}
	return nil, nil
}

func (f *fakePlatformStore) projectByID(ctx context.Context, subject, projectID string) (projectRecord, error) {
	if f.projectByIDFn != nil {
		return f.projectByIDFn(ctx, subject, projectID)
	}
	return projectRecord{}, sql.ErrNoRows
}

func (f *fakePlatformStore) createScheduledService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	if f.createScheduledServiceFn != nil {
		return f.createScheduledServiceFn(ctx, subject, projectID, name, spec)
	}
	return serviceRecord{ID: "service-1", ProjectID: projectID, Name: name, Spec: spec, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) updateService(ctx context.Context, subject, projectID, serviceID string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	if f.updateServiceFn != nil {
		return f.updateServiceFn(ctx, subject, projectID, serviceID, spec)
	}
	return serviceRecord{}, false, nil
}

func (f *fakePlatformStore) redeployService(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	if f.redeployServiceFn != nil {
		return f.redeployServiceFn(ctx, subject, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) deleteService(ctx context.Context, subject, projectID, serviceID string) error {
	if f.deleteServiceFn != nil {
		return f.deleteServiceFn(ctx, subject, projectID, serviceID)
	}
	return nil
}

func (f *fakePlatformStore) serviceByID(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	if f.serviceByIDFn != nil {
		return f.serviceByIDFn(ctx, subject, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) listServices(ctx context.Context, subject, projectID string) ([]serviceRecord, error) {
	if f.listServicesFn != nil {
		return f.listServicesFn(ctx, subject, projectID)
	}
	return nil, nil
}

func (f *fakePlatformStore) createScheduledVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error) {
	if f.createScheduledVolumeFn != nil {
		return f.createScheduledVolumeFn(ctx, subject, projectID, name, sizeBytes)
	}
	return volumeRecord{ID: "volume-1", ProjectID: projectID, Name: name, SizeBytes: sizeBytes, BoundAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) listVolumes(ctx context.Context, subject, projectID string) ([]volumeRecord, error) {
	if f.listVolumesFn != nil {
		return f.listVolumesFn(ctx, subject, projectID)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteVolume(ctx context.Context, subject, projectID, volumeID string) error {
	if f.deleteVolumeFn != nil {
		return f.deleteVolumeFn(ctx, subject, projectID, volumeID)
	}
	return nil
}

func (f *fakePlatformStore) createDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string) (domainBindingRecord, bool, error) {
	if f.createDomainBindingFn != nil {
		return f.createDomainBindingFn(ctx, subject, projectID, hostname, serviceID)
	}
	return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID}, true, nil
}

func (f *fakePlatformStore) updateDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string) (domainBindingRecord, bool, error) {
	if f.updateDomainBindingFn != nil {
		return f.updateDomainBindingFn(ctx, subject, projectID, hostname, serviceID)
	}
	return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID}, false, nil
}

func (f *fakePlatformStore) domainBindingByHostname(ctx context.Context, subject, projectID, hostname string) (domainBindingRecord, error) {
	if f.domainBindingByHostFn != nil {
		return f.domainBindingByHostFn(ctx, subject, projectID, hostname)
	}
	return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: "service-1"}, nil
}

func (f *fakePlatformStore) listDomainBindings(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error) {
	if f.listDomainBindingsFn != nil {
		return f.listDomainBindingsFn(ctx, subject, projectID, serviceID)
	}
	return nil, nil
}

func (f *fakePlatformStore) deleteDomainBinding(ctx context.Context, subject, projectID, hostname string) (bool, error) {
	if f.deleteDomainBindingFn != nil {
		return f.deleteDomainBindingFn(ctx, subject, projectID, hostname)
	}
	return true, nil
}

func (f *fakePlatformStore) serviceStatus(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
	if f.serviceStatusFn != nil {
		return f.serviceStatusFn(ctx, subject, projectID, serviceID)
	}
	return serviceRecord{}, allocationRecord{}, nil
}

func (f *fakePlatformStore) listAgents(ctx context.Context) ([]agentRecord, error) {
	if f.listAgentsFn != nil {
		return f.listAgentsFn(ctx)
	}
	return []agentRecord{}, nil
}
