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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestPlatformServiceUpdateServiceMapsConcurrentUpdate(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		updateServiceFn: func(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{}, false, errConcurrentUpdate
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ProjectId: "project-1",
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
	t.Parallel()

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
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateServiceFn: func(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ProjectId: "project-1",
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
	t.Parallel()

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
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateDomainBindingFn: func(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID, TargetPort: targetPort}, true, nil
		},
	}, noopNotifier{}, ingress)

	_, err := service.UpdateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateDomainBindingRequest{
		ProjectId: "project-1",
		Hostname:  "web.example.com",
		Binding:   &platformv1.DomainBindingTarget{ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("UpdateDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceCreateDomainBindingRequiresControlPlaneTXTProof(t *testing.T) {
	t.Parallel()

	challengeDeleted := false
	store := &fakePlatformStore{
		domainOwnershipChallengeFn: func(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error) {
			return domainOwnershipChallengeRecord{
				Hostname:    hostname,
				ProjectID:   projectID,
				RecordName:  domainChallengeRecordName(hostname),
				RecordValue: domainChallengeRecordValue("proof-token"),
				ExpiresAt:   time.Now().UTC().Add(domainChallengeTTL),
			}, nil
		},
		deleteDomainOwnershipChallengeFn: func(ctx context.Context, projectID, hostname string) error {
			challengeDeleted = true
			return nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, WithDomainTXTResolver(staticTXTResolver{
		"_mesh-challenge.web.example.com.": {"ebpf-wg-mesh-domain-verification=proof-token"},
	}))

	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		ProjectId: "project-1",
		Binding:   &platformv1.DomainBindingInput{Hostname: "Web.Example.com.", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetHostname() != "web.example.com" {
		t.Fatalf("expected canonical hostname, got %q", binding.GetHostname())
	}
	if !challengeDeleted {
		t.Fatal("expected successful proof challenge to be consumed")
	}
}

func TestPlatformServiceCreateDomainBindingRejectsMissingTXTProof(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, WithDomainTXTResolver(staticTXTResolver{}))
	_, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		ProjectId: "project-1",
		Binding:   &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", got, err)
	}
}

type staticTXTResolver map[string][]string

func (r staticTXTResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	return append([]string(nil), r[name]...), nil
}

func TestPlatformServiceDeleteDomainBindingRequestsIngress(t *testing.T) {
	t.Parallel()

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

func TestPlatformServiceGetServiceStatusProjectsStagesFromReturnedAllocation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := NewPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
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
		ProjectId: "project-1",
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
	ensurePrincipalFn                 func(ctx context.Context, subject, email string) (principalRecord, error)
	createProjectFn                   func(ctx context.Context, subject, name string) (projectRecord, error)
	listProjectsFn                    func(ctx context.Context, subject string) ([]projectRecord, error)
	projectByIDFn                     func(ctx context.Context, subject, projectID string) (projectRecord, error)
	createScheduledServiceFn          func(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error)
	updateServiceFn                   func(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error)
	redeployServiceFn                 func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	discardServiceChangesFn           func(ctx context.Context, subject, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error)
	requestServiceSourceSyncFn        func(ctx context.Context, subject, projectID, serviceID string) error
	enqueueBuildForServiceFn          func(ctx context.Context, subject, projectID, serviceID, commitSHA string) (buildRunRecord, error)
	deleteServiceFn                   func(ctx context.Context, subject, projectID, serviceID string) error
	serviceByIDFn                     func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	listServicesFn                    func(ctx context.Context, subject, projectID string) ([]serviceRecord, error)
	createScheduledVolumeFn           func(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error)
	listVolumesFn                     func(ctx context.Context, subject, projectID string) ([]volumeRecord, error)
	deleteVolumeFn                    func(ctx context.Context, subject, projectID, volumeID string) error
	createDomainBindingFn             func(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	requestDomainOwnershipChallengeFn func(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error)
	domainOwnershipChallengeFn        func(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error)
	deleteDomainOwnershipChallengeFn  func(ctx context.Context, projectID, hostname string) error
	updateDomainBindingFn             func(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	domainBindingByHostFn             func(ctx context.Context, subject, projectID, hostname string) (domainBindingRecord, error)
	listDomainBindingsFn              func(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error)
	deleteDomainBindingFn             func(ctx context.Context, subject, projectID, hostname string) (bool, error)
	serviceStatusFn                   func(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error)
	listServiceDeploymentsFn          func(ctx context.Context, subject, projectID, serviceID string, limit int32) ([]deploymentRecord, error)
	allocationByServiceIDFn           func(ctx context.Context, serviceID string) (allocationRecord, error)
	listAgentsFn                      func(ctx context.Context) ([]agentRecord, error)
}

func (f *fakePlatformStore) ensurePrincipal(ctx context.Context, subject, email string) (principalRecord, error) {
	if f.ensurePrincipalFn != nil {
		return f.ensurePrincipalFn(ctx, subject, email)
	}
	return principalRecord{Subject: subject, Email: email, CreatedAt: time.Now().UTC()}, nil
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

func (f *fakePlatformStore) updateService(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	if f.updateServiceFn != nil {
		return f.updateServiceFn(ctx, subject, projectID, serviceID, name, spec)
	}
	return serviceRecord{}, false, nil
}

func (f *fakePlatformStore) redeployService(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	if f.redeployServiceFn != nil {
		return f.redeployServiceFn(ctx, subject, projectID, serviceID)
	}
	return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) discardServiceChanges(ctx context.Context, subject, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error) {
	if f.discardServiceChangesFn != nil {
		return f.discardServiceChangesFn(ctx, subject, projectID, serviceID, changeIDs, discardAll)
	}
	return serviceRecord{ID: serviceID, ProjectID: projectID, AllocatedAgentID: "node-1"}, nil
}

func (f *fakePlatformStore) requestServiceSourceSync(ctx context.Context, subject, projectID, serviceID string) error {
	if f.requestServiceSourceSyncFn != nil {
		return f.requestServiceSourceSyncFn(ctx, subject, projectID, serviceID)
	}
	return nil
}

func (f *fakePlatformStore) enqueueBuildForService(ctx context.Context, subject, projectID, serviceID, commitSHA string) (buildRunRecord, error) {
	if f.enqueueBuildForServiceFn != nil {
		return f.enqueueBuildForServiceFn(ctx, subject, projectID, serviceID, commitSHA)
	}
	return buildRunRecord{ID: "build-1", ServiceID: serviceID, ProjectID: projectID, CommitSHA: commitSHA, State: buildStateQueued}, nil
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

func (f *fakePlatformStore) createDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if f.createDomainBindingFn != nil {
		return f.createDomainBindingFn(ctx, subject, projectID, hostname, serviceID, targetPort)
	}
	return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID, TargetPort: targetPort}, true, nil
}

func (f *fakePlatformStore) requestDomainOwnershipChallenge(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error) {
	if f.requestDomainOwnershipChallengeFn != nil {
		return f.requestDomainOwnershipChallengeFn(ctx, subject, projectID, hostname)
	}
	return domainOwnershipChallengeRecord{
		Hostname: hostname, ProjectID: projectID,
		RecordName:  domainChallengeRecordName(hostname),
		RecordValue: domainChallengeRecordValue("test-token"),
		ExpiresAt:   time.Now().UTC().Add(domainChallengeTTL),
	}, nil
}

func (f *fakePlatformStore) domainOwnershipChallenge(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error) {
	if f.domainOwnershipChallengeFn != nil {
		return f.domainOwnershipChallengeFn(ctx, subject, projectID, hostname)
	}
	return domainOwnershipChallengeRecord{
		Hostname: hostname, ProjectID: projectID,
		RecordName:  domainChallengeRecordName(hostname),
		RecordValue: domainChallengeRecordValue("test-token"),
		ExpiresAt:   time.Now().UTC().Add(domainChallengeTTL),
	}, nil
}

func (f *fakePlatformStore) deleteDomainOwnershipChallenge(ctx context.Context, projectID, hostname string) error {
	if f.deleteDomainOwnershipChallengeFn != nil {
		return f.deleteDomainOwnershipChallengeFn(ctx, projectID, hostname)
	}
	return nil
}

func (f *fakePlatformStore) updateDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if f.updateDomainBindingFn != nil {
		return f.updateDomainBindingFn(ctx, subject, projectID, hostname, serviceID, targetPort)
	}
	return domainBindingRecord{Hostname: hostname, ProjectID: projectID, ServiceID: serviceID, TargetPort: targetPort}, false, nil
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

func (f *fakePlatformStore) listServiceDeployments(ctx context.Context, subject, projectID, serviceID string, limit int32) ([]deploymentRecord, error) {
	if f.listServiceDeploymentsFn != nil {
		return f.listServiceDeploymentsFn(ctx, subject, projectID, serviceID, limit)
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
