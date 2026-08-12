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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestPlatformServiceListProjectsUsesDelegatedUser(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		listProjectsFn: func(ctx context.Context, userID string) ([]projectRecord, error) {
			if userID != "user-1" {
				t.Fatalf("unexpected userID %q", userID)
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

func TestPlatformServiceRejectsViewerWrites(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		authorizeProjectWriteFn: func(context.Context, string, string) error {
			return sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{})
	_, err := service.DeleteService(
		contextWithDelegatedUser("user-1", ""),
		&platformv1.DeleteServiceRequest{ServiceId: "service-1"},
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestPlatformServiceCreateServiceMapsPlacementErrors(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		createScheduledServiceFn: func(ctx context.Context, userID, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
			return serviceRecord{}, errNoPlacementAvailable
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		EnvironmentId: "project-1",
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestPlatformServiceRejectsUnsafeHTTPHealthPaths(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{})
	for _, path := range []string{"healthz", "//redirect.example/healthz", "/healthz\r\nX-Test: injected"} {
		_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
			EnvironmentId: "project-1",
			Service: &platformv1.ServiceInput{Name: "web", Spec: directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
				Ports:       runtimePortsFromInts([]int32{8080}),
				HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: path},
			})},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("path %q: expected InvalidArgument, got %v", path, err)
		}
	}
}

func TestPlatformServiceAcceptsOnlyRolloutHTTPHealthChecks(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{})
	tests := []struct {
		name  string
		check *platformv1.HealthCheck
	}{
		{
			name: "tcp",
			check: &platformv1.HealthCheck{
				Type: platformv1.HealthCheck_TYPE_TCP,
			},
		},
		{
			name: "continuous interval",
			check: &platformv1.HealthCheck{
				Type:            platformv1.HealthCheck_TYPE_HTTP,
				Path:            "/healthz",
				IntervalSeconds: 5,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
				EnvironmentId: "project-1",
				Service: &platformv1.ServiceInput{Name: "web", Spec: directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
					Ports:       runtimePortsFromInts([]int32{8080}),
					HealthCheck: test.check,
				})},
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestPlatformServiceUpdateServiceMapsConcurrentUpdate(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		updateServiceFn: func(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{}, false, errConcurrentUpdate
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("expected Aborted, got %v", err)
	}
}

func TestPlatformServiceDeleteVolumeReturnsNotFound(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		listVolumesFn: func(ctx context.Context, userID, projectID string) ([]volumeRecord, error) {
			return []volumeRecord{}, nil
		},
	}, noopNotifier{}, noopIngress{})

	_, err := service.DeleteVolume(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteVolumeRequest{
		VolumeId: "missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPlatformServiceGetProjectMapsMissingProject(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		projectByIDFn: func(ctx context.Context, userID, projectID string) (projectRecord, error) {
			return projectRecord{}, sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{})

	_, err := service.GetProject(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetProjectRequest{})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPlatformServiceUpdateServiceSkipsIngressRequest(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateServiceFn: func(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: directImageServiceSpec("nginx:1.27", nil),
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
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return []domainBindingRecord{{Hostname: "web.example.com", EnvironmentID: projectID, ServiceID: serviceID}}, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
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
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return nil, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
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
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateDomainBindingFn: func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateDomainBindingRequest{
		Hostname: "web.example.com",
		Binding:  &platformv1.DomainBindingTarget{ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("UpdateDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceGenerateDomainBindingCreatesStablePlatformHostname(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		createPlatformDomainBindingFn: func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort, PlatformGenerated: true}, true, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"))

	binding, err := service.GenerateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GenerateDomainBindingRequest{
		ServiceId:  "service-1",
		TargetPort: 8080,
	})
	if err != nil {
		t.Fatalf("GenerateDomainBinding: %v", err)
	}
	if !strings.HasSuffix(binding.GetHostname(), ".platform.example") {
		t.Fatalf("expected platform hostname, got %q", binding.GetHostname())
	}
	if !binding.GetPlatformGenerated() {
		t.Fatal("expected generated binding marker")
	}
}

func TestPlatformServiceCreateDomainBindingVerifiesCNAMEToPlatformHostname(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
		"web.example.com": "violet-7k3.platform.example.",
	}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetHostname() != "web.example.com" {
		t.Fatalf("unexpected hostname %q", binding.GetHostname())
	}
}

func TestPlatformServiceCreateDomainBindingRejectsWrongCNAME(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
		"web.example.com": "wrong.platform.example.",
	}))
	_, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", got, err)
	}
}

type staticCNAMEResolver map[string]string

func (r staticCNAMEResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if target, ok := r[name]; ok {
		return target, nil
	}
	return "", errors.New("not found")
}

func TestPlatformServiceDeleteDomainBindingRequestsIngress(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		deleteDomainBindingFn: func(ctx context.Context, userID, projectID, hostname string) (bool, error) {
			return true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.DeleteDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteDomainBindingRequest{
		Hostname: "web.example.com",
	})
	if err != nil {
		t.Fatalf("DeleteDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceGetServiceStatusProjectsStagesFromReturnedAllocation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := NewPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
			return serviceRecord{
					ID:               serviceID,
					ProjectID:        projectID,
					AllocatedAgentID: "node-1",
					CreatedAt:        now.Add(-10 * time.Minute),
					Spec:             directImageServiceSpec("nginx:1.27", nil),
					LatestBuild: &platformv1.BuildStatus{
						BuildId: "build-1",
					},
				}, allocationRecord{
					ID:                       "alloc-status",
					ServiceID:                serviceID,
					AgentID:                  "node-1",
					DesiredRolloutGeneration: 2,
					AppliedRolloutGeneration: 1,
					Healthy:                  false,
					UpdatedAt:                now,
				}, nil
		},
		allocationByServiceIDFn: func(ctx context.Context, serviceID string) (allocationRecord, error) {
			return allocationRecord{
				ID:                       "alloc-newer",
				ServiceID:                serviceID,
				AgentID:                  "node-1",
				DesiredRolloutGeneration: 2,
				AppliedRolloutGeneration: 2,
				Healthy:                  true,
				UpdatedAt:                now,
			}, nil
		},
	}, noopNotifier{}, noopIngress{})

	resp, err := service.GetServiceStatus(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetServiceStatusRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if got := resp.GetAllocation().GetAppliedRolloutGeneration(); got != 1 {
		t.Fatalf("expected returned allocation snapshot to stay stale for test, got %d", got)
	}
	stages := resp.GetService().GetLatestBuild().GetStages()
	if len(stages) == 0 {
		t.Fatalf("expected projected stages")
	}
	postDeploy := stages[len(stages)-1]
	if postDeploy.GetKey() != StagePostDeploy {
		t.Fatalf("expected last stage post-deploy, got %q", postDeploy.GetKey())
	}
	if postDeploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING {
		t.Fatalf("expected post-deploy to use returned allocation snapshot, got %v", postDeploy.GetState())
	}
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
	serviceStatusFn                   func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, allocationRecord, error)
	listServiceDeploymentsFn          func(ctx context.Context, userID, projectID, serviceID string, limit int32) ([]deploymentRecord, error)
	allocationByServiceIDFn           func(ctx context.Context, serviceID string) (allocationRecord, error)
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

func (f *fakePlatformStore) serviceStatus(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
	if f.serviceStatusFn != nil {
		return f.serviceStatusFn(ctx, userID, projectID, serviceID)
	}
	return serviceRecord{}, allocationRecord{}, nil
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

func (f *fakePlatformStore) listAgents(ctx context.Context) ([]agentRecord, error) {
	if f.listAgentsFn != nil {
		return f.listAgentsFn(ctx)
	}
	return []agentRecord{}, nil
}
