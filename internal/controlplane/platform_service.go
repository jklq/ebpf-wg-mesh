package controlplane

import (
	"context"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

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
	redeployService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
	restartService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error)
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
