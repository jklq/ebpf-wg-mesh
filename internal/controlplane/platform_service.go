package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type PlatformService struct {
	platformv1.UnimplementedPlatformServiceServer
	store                platformStore
	environmentStore     environmentStore
	logStore             serviceLogStore
	emitter              *LogEmitter
	notifier             platformNotifier
	ingress              platformIngress
	inspector            *gitHubSourceInspector
	dnsResolver          domainCNAMEResolver
	platformDomainSuffix string
	events               *PlatformEvents
}

type platformStore interface {
	environmentStore
	createProject(ctx context.Context, userID, name string) (projectRecord, error)
	listProjects(ctx context.Context, userID string) ([]projectRecord, error)
	projectByID(ctx context.Context, userID, projectID string) (projectRecord, error)
	authorizeProjectWrite(ctx context.Context, userID, projectID string) error
	createScheduledService(ctx context.Context, userID, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error)
	updateService(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error)
	applyDeploymentAction(ctx context.Context, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (serviceRecord, deploymentActionRecord, error)
	discardServiceChanges(ctx context.Context, userID, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error)
	requestServiceSourceSync(ctx context.Context, userID, projectID, serviceID string) error
	enqueueBuildForService(ctx context.Context, userID, projectID, serviceID, commitSHA string) (buildRunRecord, error)
	deleteService(ctx context.Context, userID, projectID, serviceID string) error
	serviceByID(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
	listServices(ctx context.Context, userID, projectID string) ([]serviceRecord, error)
	createScheduledVolume(ctx context.Context, userID, projectID, name string, sizeBytes int64) (volumeRecord, error)
	listVolumes(ctx context.Context, userID, projectID string) ([]volumeRecord, error)
	deleteVolume(ctx context.Context, userID, projectID, volumeID string) error
	createDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	createPlatformDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	platformDomainBindingForService(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error)
	updateDomainBinding(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	domainBindingByHostname(ctx context.Context, userID, projectID, hostname string) (domainBindingRecord, error)
	listDomainBindings(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error)
	deleteDomainBinding(ctx context.Context, userID, projectID, hostname string) (bool, error)
	serviceStatus(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, []allocationRecord, error)
	scaleService(ctx context.Context, userID, projectID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error)
	listServiceDeployments(ctx context.Context, userID, projectID, serviceID string, limit int32) ([]deploymentRecord, error)
	allocationByServiceID(ctx context.Context, serviceID string) (allocationRecord, error)
	listAllocationsByServiceID(ctx context.Context, serviceID string) ([]allocationRecord, error)
	listAgents(ctx context.Context) ([]agentRecord, error)
}

type environmentStore interface {
	listEnvironments(ctx context.Context, userID, projectID string) ([]environmentRecord, error)
	environmentByID(ctx context.Context, userID, environmentID string) (environmentRecord, error)
	createEnvironment(ctx context.Context, userID, projectID, name string) (environmentRecord, error)
	duplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (environmentRecord, error)
	renameEnvironment(ctx context.Context, userID, environmentID, name string) (environmentRecord, error)
	deleteEnvironment(ctx context.Context, userID, environmentID string) ([]string, error)
	deployEnvironment(ctx context.Context, userID, environmentID string) ([]serviceRecord, []string, error)
}

func (s *PlatformService) environments() (environmentStore, error) {
	return s.store, nil
}

func (s *PlatformService) environmentForUser(ctx context.Context, userID, environmentID string) (environmentRecord, error) {
	store, err := s.environments()
	if err != nil {
		return environmentRecord{}, err
	}
	return store.environmentByID(ctx, userID, environmentID)
}

type platformNotifier interface {
	Notify(agentID string)
}

type platformIngress interface {
	Sync(ctx context.Context) error
	RequestSync()
}

type serviceLogStore interface {
	ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) ([]serviceLogRecord, error)
}

type PlatformServiceOption func(*PlatformService)

type domainCNAMEResolver interface {
	LookupCNAME(context.Context, string) (string, error)
	LookupHost(context.Context, string) ([]string, error)
}

func WithDomainCNAMEResolver(resolver domainCNAMEResolver) PlatformServiceOption {
	return func(service *PlatformService) {
		service.dnsResolver = resolver
	}
}

func WithPlatformDomainSuffix(suffix string) PlatformServiceOption {
	return func(service *PlatformService) {
		service.platformDomainSuffix = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(suffix), "."))
	}
}

func WithServiceLogs(logStore serviceLogStore) PlatformServiceOption {
	return func(service *PlatformService) {
		service.logStore = logStore
	}
}

// WithServiceLogEmitter wires a LogEmitter so the platform service can write
// synthetic deploy/initialization log lines (for example, when a service is
// first scheduled or redeployed). A nil emitter is a valid no-op.
func WithServiceLogEmitter(emitter *LogEmitter) PlatformServiceOption {
	return func(service *PlatformService) {
		service.emitter = emitter
	}
}

func WithGitHubSourceInspection(catalog *GitHubCatalog, client *GitHubClient) PlatformServiceOption {
	return func(service *PlatformService) {
		service.inspector = newGitHubSourceInspector(catalog, client)
	}
}

func WithPlatformEvents(events *PlatformEvents) PlatformServiceOption {
	return func(service *PlatformService) {
		service.events = events
	}
}

func NewPlatformService(store platformStore, notifier platformNotifier, ingress platformIngress, opts ...PlatformServiceOption) *PlatformService {
	service := &PlatformService{store: store, environmentStore: store, notifier: notifier, ingress: ingress, dnsResolver: newPublicDNSResolver()}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *PlatformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.createProject(ctx, identity.UserID, req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create project: %v", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) ListProjects(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListProjectsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listProjects(ctx, identity.UserID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list projects: %v", err)
	}
	resp := &platformv1.ListProjectsResponse{Projects: make([]*platformv1.Project, 0, len(items))}
	for _, item := range items {
		resp.Projects = append(resp.Projects, toProtoProject(item))
	}
	return resp, nil
}

func (s *PlatformService) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.projectByID(ctx, identity.UserID, req.GetProjectId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "project: %v", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) ListEnvironments(ctx context.Context, req *platformv1.ListEnvironmentsRequest) (*platformv1.ListEnvironmentsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.environmentStore.listEnvironments(ctx, identity.UserID, req.GetProjectId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "list environments: %v", err)
	}
	resp := &platformv1.ListEnvironmentsResponse{Environments: make([]*platformv1.Environment, 0, len(items))}
	for _, item := range items {
		resp.Environments = append(resp.Environments, toProtoEnvironment(item))
	}
	return resp, nil
}

func (s *PlatformService) GetEnvironment(ctx context.Context, req *platformv1.GetEnvironmentRequest) (*platformv1.Environment, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.environmentStore.environmentByID(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) CreateEnvironment(ctx context.Context, req *platformv1.CreateEnvironmentRequest) (*platformv1.Environment, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.environmentStore.createEnvironment(ctx, identity.UserID, req.GetProjectId(), req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) DuplicateEnvironment(ctx context.Context, req *platformv1.DuplicateEnvironmentRequest) (*platformv1.Environment, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.environmentStore.duplicateEnvironment(ctx, identity.UserID, req.GetSourceEnvironmentId(), req.GetName(), req.GetCopyVariables())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "duplicate environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) RenameEnvironment(ctx context.Context, req *platformv1.RenameEnvironmentRequest) (*platformv1.Environment, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.environmentStore.renameEnvironment(ctx, identity.UserID, req.GetEnvironmentId(), req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "rename environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.environmentStore.deleteEnvironment(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		if errors.Is(err, errProductionEnvironment) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "delete environment: %v", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	s.events.Publish(req.GetEnvironmentId())
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) DeployEnvironment(ctx context.Context, req *platformv1.DeployEnvironmentRequest) (*platformv1.DeployEnvironmentResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	services, agentIDs, err := s.environmentStore.deployEnvironment(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "deploy environment: %v", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	resp := &platformv1.DeployEnvironmentResponse{Services: make([]*platformv1.ServiceStatus, 0, len(services))}
	for _, service := range services {
		allocations, err := s.store.listAllocationsByServiceID(ctx, service.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load deployed allocations: %v", err)
		}
		resp.Services = append(resp.Services, toProtoServiceStatus(service, allocations, 0))
	}
	s.events.Publish(req.GetEnvironmentId())
	return resp, nil
}

func (s *PlatformService) LinkGitHubRepository(ctx context.Context, req *platformv1.LinkGitHubRepositoryRequest) (*platformv1.InspectSourceResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetRepositorySelector()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project and repository selector are required")
	}
	if s.inspector == nil {
		return nil, status.Error(codes.FailedPrecondition, "github source inspection is not configured")
	}
	resp, err := s.inspector.LinkAndInspect(ctx, req.GetProjectId(), identity.UserID, req.GetRepositorySelector(), req.GetGithubUserAccessToken())
	if err != nil {
		if authErr := gitHubUserAuthorizationStatus(err); authErr != nil {
			return nil, authErr
		}
		return nil, status.Errorf(codes.Internal, "link github repository: %v", err)
	}
	return resp, nil
}

func (s *PlatformService) InspectSource(ctx context.Context, req *platformv1.InspectSourceRequest) (*platformv1.InspectSourceResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(strings.ToLower(req.GetProvider())) != "github" {
		return nil, status.Error(codes.InvalidArgument, "unsupported source provider")
	}
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetRepositorySelector()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project and repository selector are required")
	}
	if _, err := s.store.projectByID(ctx, identity.UserID, req.GetProjectId()); err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "project access: %v", err)
	}
	if s.inspector == nil {
		return nil, status.Error(codes.FailedPrecondition, "github source inspection is not configured")
	}
	resp, err := s.inspector.Inspect(ctx, req.GetProjectId(), req.GetRepositorySelector(), req.GetGithubUserAccessToken())
	if err != nil {
		if authErr := gitHubUserAuthorizationStatus(err); authErr != nil {
			return nil, authErr
		}
		return nil, status.Errorf(codes.Internal, "inspect source: %v", err)
	}
	return resp, nil
}

func gitHubUserAuthorizationStatus(err error) error {
	var authErr *gitHubUserRepositoryAuthorizationError
	if !errors.As(err, &authErr) {
		return nil
	}
	if errors.Is(authErr, errGitHubUserAccessTokenRequired) {
		return status.Error(codes.Unauthenticated, "GitHub user authorization is required")
	}
	var apiErr *gitHubAPIError
	if errors.As(authErr, &apiErr) {
		switch apiErr.StatusCode {
		case 401:
			return status.Error(codes.Unauthenticated, "GitHub user authorization is invalid or expired")
		case 403, 404:
			return status.Error(codes.PermissionDenied, "the signed-in GitHub user cannot access this repository")
		default:
			return status.Error(codes.Unavailable, "GitHub user authorization is temporarily unavailable")
		}
	}
	if errors.Is(authErr, errGitHubRepositoryIdentityMismatch) {
		return status.Error(codes.PermissionDenied, "GitHub repository identity could not be verified")
	}
	return status.Error(codes.Unavailable, "GitHub user authorization is temporarily unavailable")
}

func (s *PlatformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := canonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	environment, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	if err := s.authorizeServiceSource(ctx, environment.ProjectID, spec); err != nil {
		return nil, err
	}
	var service serviceRecord
	service, err = s.store.createScheduledService(ctx, identity.UserID, req.GetEnvironmentId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) || errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create service: %v", err)
	}
	slog.Info("service created", "service_id", service.ID, "environment_id", service.EnvironmentID, "spec_revision", service.SpecRevision, "rollout_generation", service.RolloutGeneration)
	service, err = s.store.serviceByID(ctx, identity.UserID, "", service.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload service: %v", err)
	}
	// Emit an initialization-stage line so the right panel immediately shows
	// activity. For image-based services this is the only pre-build step; for
	// source-based services the github/build coordinators will emit follow-up
	// lines as the pipeline progresses.
	s.emitInitialization(ctx, service)
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	s.events.Publish(service.EnvironmentID)
	return toProtoService(service), nil
}

// emitInitialization writes a single synthetic log line describing the initial
// service scheduling decision. It is a small convenience that keeps the
// platform service methods concise and the phrasing consistent across create
// and redeploy paths.
func (s *PlatformService) emitInitialization(ctx context.Context, service serviceRecord) {
	if s.emitter == nil || !s.emitter.Enabled() {
		return
	}
	agentID := service.AllocatedAgentID
	if agentID == "" {
		agentID = "pending placement"
	}
	s.emitter.EmitDeployf(ctx, service, "", "", StageInitialization, "Service scheduled on agent %s (rollout %d)", agentID, service.RolloutGeneration)
}

func (s *PlatformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := canonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	if err := s.authorizeServiceSource(ctx, current.ProjectID, spec); err != nil {
		return nil, err
	}
	service, _, err := s.store.updateService(ctx, identity.UserID, "", req.GetServiceId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "update service: %v", err)
		}
		if errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) || errors.Is(err, errVolumeReplicaUnsupported) {
			return nil, status.Errorf(codes.FailedPrecondition, "update service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update service: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	s.events.Publish(service.EnvironmentID)
	return toProtoService(service), nil
}

func (s *PlatformService) ApplyDeploymentAction(ctx context.Context, req *platformv1.ApplyDeploymentActionRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" || strings.TrimSpace(req.GetDeploymentId()) == "" ||
		strings.TrimSpace(req.GetIdempotencyKey()) == "" || deploymentActionName(req.GetAction()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id, deployment_id, action, and idempotency_key are required")
	}
	currentService, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, currentService.ProjectID); err != nil {
		return nil, err
	}
	_, _, err = s.store.applyDeploymentAction(ctx, identity.UserID, req.GetServiceId(), req.GetDeploymentId(), req.GetAction(), req.GetIdempotencyKey(), req.GetAllocationId())
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Errorf(codes.NotFound, "deployment action target: %v", err)
		case errors.Is(err, errDeploymentActionConflict), errors.Is(err, errConcurrentUpdate):
			return nil, status.Errorf(codes.Aborted, "deployment action: %v", err)
		case errors.Is(err, errDeploymentActionInvalid), errors.Is(err, errDeploymentStale),
			errors.Is(err, errInvalidReplicaCount), errors.Is(err, errVolumeReplicaUnsupported):
			return nil, status.Errorf(codes.FailedPrecondition, "deployment action: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "deployment action: %v", err)
		}
	}
	s.notifyAllAgents(ctx)
	s.ingress.RequestSync()
	currentService, allocations, err := s.store.serviceStatus(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deployment action status: %v", err)
	}
	currentService, err = s.decorateServiceRecordWithAllocations(ctx, currentService, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate deployment action status: %v", err)
	}
	index := s.events.Publish(currentService.EnvironmentID)
	return toProtoServiceStatus(currentService, allocations, index), nil
}

func (s *PlatformService) ScaleService(ctx context.Context, req *platformv1.ScaleServiceRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, allocations, err := s.store.scaleService(ctx, identity.UserID, "", req.GetServiceId(), req.GetDesiredReplicaCount())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "scale service: %v", err)
		}
		if errors.Is(err, errInvalidReplicaCount) || errors.Is(err, errVolumeReplicaUnsupported) {
			return nil, status.Errorf(codes.FailedPrecondition, "scale service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "scale service: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate scaled service: %v", err)
	}
	index := s.events.Publish(service.EnvironmentID)
	return toProtoServiceStatus(service, allocations, index), nil
}

func (s *PlatformService) DiscardServiceChanges(ctx context.Context, req *platformv1.DiscardServiceChangesRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, err := s.store.discardServiceChanges(ctx, identity.UserID, "", req.GetServiceId(), req.GetChangeIds(), req.GetDiscardAll())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "discard service changes: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "discard service changes: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	s.events.Publish(service.EnvironmentID)
	return toProtoService(service), nil
}

func (s *PlatformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, service.ProjectID); err != nil {
		return nil, err
	}
	bindings, err := s.store.listDomainBindings(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	if err := s.store.deleteService(ctx, identity.UserID, "", req.GetServiceId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete service: %v", err)
	}
	s.notifyAllAgents(ctx)
	if len(bindings) > 0 {
		s.ingress.RequestSync()
	}
	s.events.Publish(service.EnvironmentID)
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *PlatformService) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	environment, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	index, changed := s.events.Wait(ctx, environment.ID, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !changed {
		return &platformv1.ListServicesResponse{Index: index, NotModified: true}, nil
	}
	items, err := s.store.listServices(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list services: %v", err)
	}
	resp := &platformv1.ListServicesResponse{Services: make([]*platformv1.Service, 0, len(items)), Index: index}
	for _, item := range items {
		item, err = s.decorateServiceRecord(ctx, item)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
		}
		resp.Services = append(resp.Services, toProtoService(item))
	}
	return resp, nil
}

func (s *PlatformService) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	volume, err := s.store.createScheduledVolume(ctx, identity.UserID, req.GetEnvironmentId(), req.GetName(), req.GetSizeBytes())
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "create volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}
	slog.Info("volume created", "volume_id", volume.ID, "environment_id", volume.EnvironmentID)
	return toProtoVolume(volume), nil
}

func (s *PlatformService) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteVolume(ctx, identity.UserID, "", req.GetVolumeId()); err != nil {
		if errors.Is(err, errVolumeInUse) {
			return nil, status.Errorf(codes.FailedPrecondition, "delete volume: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete volume: %v", err)
	}
	s.notifyAllAgents(ctx)
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listVolumes(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	resp := &platformv1.ListVolumesResponse{Volumes: make([]*platformv1.Volume, 0, len(items))}
	for _, item := range items {
		resp.Volumes = append(resp.Volumes, toProtoVolume(item))
	}
	return resp, nil
}

func (s *PlatformService) CreateDomainBinding(ctx context.Context, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetBinding().GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := canonicalDomainHostname(req.GetBinding().GetHostname())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hostname: %v", err)
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	if isPlatformHostname(hostname, s.platformDomainSuffix) {
		return nil, status.Error(codes.InvalidArgument, "hostname is reserved for generated platform domains")
	}
	if _, err := s.store.platformDomainBindingForService(ctx, identity.UserID, service.ProjectID, req.GetBinding().GetServiceId()); err != nil {
		if errors.Is(err, errPlatformDomainNotGenerated) || errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.FailedPrecondition, "domain ownership: %v", errPlatformDomainNotGenerated)
		}
		return nil, status.Errorf(codes.Internal, "load platform domain: %v", err)
	}
	binding, changed, err := s.store.createDomainBinding(ctx, identity.UserID, service.ProjectID, hostname, req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create domain binding: %v", err)
		}
		if errors.Is(err, errInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		s.events.Publish(service.EnvironmentID)
	}
	return s.annotateDomainBinding(ctx, identity.UserID, service.ProjectID, binding), nil
}

func (s *PlatformService) GenerateDomainBinding(ctx context.Context, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetPort := req.GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := generatedPlatformHostname(service.ProjectID, req.GetServiceId(), s.platformDomainSuffix)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "generate platform hostname: %v", err)
	}
	binding, changed, err := s.store.createPlatformDomainBinding(ctx, identity.UserID, service.ProjectID, hostname, req.GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "generate domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "generate domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		s.events.Publish(service.EnvironmentID)
	}
	return s.annotateDomainBinding(ctx, identity.UserID, service.ProjectID, binding), nil
}

func (s *PlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding.ProjectID, binding), nil
}

func (s *PlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listDomainBindings(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	resp := &platformv1.ListDomainBindingsResponse{Bindings: make([]*platformv1.DomainBinding, 0, len(items))}
	for _, item := range items {
		resp.Bindings = append(resp.Bindings, s.annotateDomainBinding(ctx, identity.UserID, item.ProjectID, item))
	}
	return resp, nil
}

func (s *PlatformService) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	var previousServiceID string
	if previous, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname()); err == nil {
		previousServiceID = previous.ServiceID
	}
	binding, changed, err := s.store.updateDomainBinding(ctx, identity.UserID, "", req.GetHostname(), req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		if errors.Is(err, errInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", previousServiceID, binding.ServiceID)
		s.ingress.RequestSync()
		if updated, err := s.store.serviceByID(ctx, identity.UserID, "", binding.ServiceID); err == nil {
			s.events.Publish(updated.EnvironmentID)
		}
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding.ProjectID, binding), nil
}

func (s *PlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	if binding.PlatformGenerated {
		items, listErr := s.store.listDomainBindings(ctx, identity.UserID, "", binding.ServiceID)
		if listErr != nil {
			return nil, status.Errorf(codes.Internal, "list domain bindings: %v", listErr)
		}
		if hasCustomDomainBinding(items) {
			return nil, status.Errorf(codes.FailedPrecondition, "delete domain binding: %v", errPlatformDomainInUse)
		}
	}
	changed, err := s.store.deleteDomainBinding(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain binding: %v", err)
	}
	if !binding.PlatformGenerated {
		items, listErr := s.store.listDomainBindings(ctx, identity.UserID, "", binding.ServiceID)
		if listErr != nil {
			return nil, status.Errorf(codes.Internal, "list domain bindings: %v", listErr)
		}
		if leftover := leftoverPlatformHostname(items); leftover != "" {
			extra, deleteErr := s.store.deleteDomainBinding(ctx, identity.UserID, "", leftover)
			if deleteErr != nil && !errors.Is(deleteErr, sql.ErrNoRows) {
				return nil, status.Errorf(codes.Internal, "delete generated domain binding: %v", deleteErr)
			}
			changed = changed || extra
		}
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		if updated, err := s.store.serviceByID(ctx, identity.UserID, "", binding.ServiceID); err == nil {
			s.events.Publish(updated.EnvironmentID)
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, allocations, err := s.store.serviceStatus(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service status: %v", err)
	}
	index, changed := s.events.Wait(ctx, service.EnvironmentID, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !changed {
		return &platformv1.ServiceStatus{Index: index, NotModified: true}, nil
	}
	// Re-read after the wait: the first read only resolves the environment to
	// watch. Returning it here would report the state from *before* the change
	// that woke us, leaving every watcher one event behind — the final "healthy"
	// status of a rollout would then never reach the client.
	service, allocations, err = s.store.serviceStatus(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service status: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service status: %v", err)
	}
	return toProtoServiceStatus(service, allocations, index), nil
}

func (s *PlatformService) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	if s.logStore == nil {
		return nil, status.Error(codes.FailedPrecondition, errLogStoreDisabled.Error())
	}
	if _, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	lines, err := s.logStore.ListServiceLogs(ctx, req)
	if err != nil {
		if errors.Is(err, errLogStoreDisabled) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "list service logs: %v", err)
	}
	resp := &platformv1.ListServiceLogsResponse{Lines: make([]*platformv1.ServiceLogLine, 0, len(lines))}
	for _, line := range lines {
		resp.Lines = append(resp.Lines, toProtoServiceLogLine(line))
	}
	return resp, nil
}

func (s *PlatformService) ListServiceDeployments(ctx context.Context, req *platformv1.ListServiceDeploymentsRequest) (*platformv1.ListServiceDeploymentsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	items, err := s.store.listServiceDeployments(ctx, identity.UserID, "", req.GetServiceId(), req.GetLimit())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service deployments: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "list service deployments: %v", err)
	}
	resp := &platformv1.ListServiceDeploymentsResponse{
		Deployments: make([]*platformv1.DeploymentRecord, 0, len(items)),
	}
	for _, item := range items {
		resp.Deployments = append(resp.Deployments, toProtoDeploymentRecord(item))
	}
	return resp, nil
}

func (s *PlatformService) ListAgents(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	items, err := s.store.listAgents(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	resp := &platformv1.ListAgentsResponse{Agents: make([]*platformv1.Agent, 0, len(items))}
	for _, item := range items {
		resp.Agents = append(resp.Agents, toProtoAgent(item))
	}
	return resp, nil
}

func (s *PlatformService) decorateServiceRecord(ctx context.Context, service serviceRecord) (serviceRecord, error) {
	return s.decorateServiceRecordWithAllocations(ctx, service, nil)
}

func (s *PlatformService) decorateServiceRecordWithAllocations(ctx context.Context, service serviceRecord, allocs []allocationRecord) (serviceRecord, error) {
	if service.SourceSummary == nil {
		service.SourceSummary = buildSourceSummary(service.Spec)
	}
	// Stages are projected from the allocations + latest build; if we cannot
	// load allocations we still return the stages derived from just the
	// service+build so the UI gets something to render (showing a "waiting"
	// deploy stage rather than a hard error).
	if allocs == nil {
		var err error
		allocs, err = s.store.listAllocationsByServiceID(ctx, service.ID)
		if err != nil {
			return serviceRecord{}, err
		}
	}
	var buildRec *buildRunRecord
	if service.LatestBuild != nil {
		rec := buildRunRecordFromProto(service.LatestBuild)
		buildRec = &rec
	}
	service.ReadyReplicaCount = countReadyAllocations(allocs)
	summary := summarizeAllocations(allocs, service.DesiredReplicaCount)
	if service.LatestDeployment == nil {
		inferred := inferDeploymentFromLegacy(service, buildRec, summary)
		service.LatestDeployment = &inferred
	}
	stages := deploymentStages(service, buildRec, summary)
	if service.LatestBuild == nil && len(stages) > 0 {
		// Stages still travel on BuildStatus for older console clients.
		service.LatestBuild = &platformv1.BuildStatus{Stages: stages}
	} else if service.LatestBuild != nil {
		service.LatestBuild.Stages = stages
	}
	return service, nil
}

func (s *PlatformService) notifyServiceAgents(ctx context.Context, serviceID string, identityCatalogChanged bool) {
	if identityCatalogChanged || s.notifier == nil {
		s.notifyAllAgents(ctx)
		return
	}
	ids, err := s.store.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		s.notifyAllAgents(ctx)
		return
	}
	seen := map[string]struct{}{}
	for _, alloc := range ids {
		if alloc.AgentID == "" {
			continue
		}
		if _, ok := seen[alloc.AgentID]; ok {
			continue
		}
		seen[alloc.AgentID] = struct{}{}
		s.notifier.Notify(alloc.AgentID)
	}
	if len(seen) == 0 {
		s.notifyAllAgents(ctx)
	}
}

// buildRunRecordFromProto rebuilds the (minimal) in-memory buildRunRecord we
// need for stage projection starting from a proto BuildStatus. We don't
// round-trip every field — the projector only reads state and timestamps, so
// we only reconstruct those. Keeping this narrow avoids accidentally widening
// the implicit contract between decorator and projector.
func buildRunRecordFromProto(status *platformv1.BuildStatus) buildRunRecord {
	rec := buildRunRecord{
		ID:            status.GetBuildId(),
		CommitSHA:     status.GetCommitSha(),
		CommitMessage: status.GetCommitMessage(),
		CommitAuthor:  status.GetCommitAuthor(),
		ImageDigest:   status.GetImageDigest(),
		FailureReason: status.GetFailureReason(),
	}
	switch status.GetState() {
	case platformv1.BuildState_BUILD_STATE_QUEUED:
		rec.State = buildStateQueued
	case platformv1.BuildState_BUILD_STATE_RUNNING:
		rec.State = buildStateRunning
	case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
		rec.State = buildStateSucceeded
	case platformv1.BuildState_BUILD_STATE_FAILED:
		rec.State = buildStateFailed
	case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
		rec.State = buildStateSuperseded
	case platformv1.BuildState_BUILD_STATE_CANCELLED:
		rec.State = buildStateCancelled
	}
	if queued := status.GetQueuedAt(); queued != nil {
		rec.QueuedAt = queued.AsTime()
	}
	if started := status.GetStartedAt(); started != nil {
		rec.StartedAt = sql.NullTime{Time: started.AsTime(), Valid: true}
	}
	if finished := status.GetFinishedAt(); finished != nil {
		rec.FinishedAt = sql.NullTime{Time: finished.AsTime(), Valid: true}
	}
	return rec
}

func validateServiceSpecRestart(spec *platformv1.ServiceSpec) error {
	runtime := spec.GetRuntime()
	if err := restartpolicy.ValidateRestart(runtime.GetRestart()); err != nil {
		return err
	}
	if check := runtime.GetLivenessCheck(); check != nil {
		if check.GetPort() > 0 {
			if err := validatePort(check.GetPort()); err != nil {
				return err
			}
		}
		if check.GetTimeoutSeconds() < 0 {
			return errors.New("liveness check timeout must be non-negative")
		}
		switch check.GetType() {
		case platformv1.HealthCheck_TYPE_UNSPECIFIED:
			if check.GetPath() != "" || check.GetPort() != 0 || check.GetTimeoutSeconds() != 0 {
				return errors.New("only explicit HTTP liveness checks are supported")
			}
		case platformv1.HealthCheck_TYPE_HTTP:
			if !validHealthCheckPath(check.GetPath()) {
				return errors.New("HTTP liveness check path must be an absolute request path beginning with one slash")
			}
		default:
			return errors.New("unsupported liveness check type")
		}
	}
	return nil
}

func validateServiceSpecPorts(spec *platformv1.ServiceSpec) error {
	runtime := spec.GetRuntime()
	for _, port := range runtime.GetPorts() {
		if err := validatePort(port.GetPort()); err != nil {
			return err
		}
	}
	check := runtime.GetHealthCheck()
	if check == nil {
		return nil
	}
	if check.GetPort() > 0 {
		if err := validatePort(check.GetPort()); err != nil {
			return err
		}
	}
	if check.GetPort() == 0 && len(runtime.GetPorts()) == 0 && check.GetType() != platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return errors.New("health check requires a port or at least one runtime port")
	}
	if check.GetTimeoutSeconds() < 0 {
		return errors.New("health check timeout must be non-negative")
	}
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_UNSPECIFIED:
		if check.GetPath() != "" || check.GetPort() != 0 || check.GetTimeoutSeconds() != 0 {
			return errors.New("only explicit HTTP health checks are supported")
		}
	case platformv1.HealthCheck_TYPE_HTTP:
		if !validHealthCheckPath(check.GetPath()) {
			return errors.New("HTTP health check path must be an absolute request path beginning with one slash")
		}
	default:
		return errors.New("unsupported health check type")
	}
	return nil
}

func validHealthCheckPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	return err == nil && !parsed.IsAbs() && parsed.Host == ""
}

func (s *PlatformService) notifyServices(ctx context.Context, userID, projectID string, serviceIDs ...string) {
	seen := make(map[string]struct{}, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		serviceID = strings.TrimSpace(serviceID)
		if serviceID == "" {
			continue
		}
		if _, ok := seen[serviceID]; ok {
			continue
		}
		seen[serviceID] = struct{}{}
		service, err := s.store.serviceByID(ctx, userID, projectID, serviceID)
		if err != nil {
			slog.Warn("failed to load service for domain notification", "service_id", serviceID, "error", err)
			continue
		}
		s.notifyServiceAgents(ctx, service.ID, false)
	}
}

func (s *PlatformService) notifyAllAgents(ctx context.Context) {
	agents, err := s.store.listAgents(ctx)
	if err != nil {
		slog.Warn("failed to list agents for cluster identity notification", "error", err)
		return
	}
	for _, agent := range agents {
		s.notifier.Notify(agent.ID)
	}
}

func (s *PlatformService) authorizeServiceSource(ctx context.Context, projectID string, spec *platformv1.ServiceSpec) error {
	if spec == nil || spec.GetSource() == nil || spec.GetSource().GetSourceSpec() == nil {
		return nil
	}
	if s.inspector == nil {
		return status.Error(codes.FailedPrecondition, "github source authorization is not configured")
	}
	if err := s.inspector.Authorize(ctx, projectID, spec.GetSource().GetSourceSpec()); err != nil {
		return status.Errorf(codes.PermissionDenied, "source authorization: %v", err)
	}
	return nil
}

func (s *PlatformService) requireProjectWriteAccess(ctx context.Context, userID, projectID string) error {
	if err := s.store.authorizeProjectWrite(ctx, userID, projectID); err != nil {
		return status.Errorf(codes.PermissionDenied, "project write access: %v", err)
	}
	return nil
}
