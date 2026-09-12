package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/controlplane/source"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type PlatformService struct {
	platformv1.UnimplementedPlatformServiceServer
	store                platformStore
	domains              *routing.Domains
	environments         *EnvironmentOperations
	delivery             platformDelivery
	logStore             serviceLogStore
	emitter              *logs.LogEmitter
	notifier             deliverycore.PlatformNotifier
	ingress              deliverycore.PlatformIngress
	inspector            *source.Inspector
	dnsResolver          routing.Resolver
	platformDomainSuffix string
	events               *PlatformEvents
	liveOwner            LiveOwner
}

type platformStore interface {
	environmentStore
	routing.Store
	createProject(ctx context.Context, userID, name string) (deliverycore.ProjectRecord, error)
	listProjects(ctx context.Context, userID string) ([]deliverycore.ProjectRecord, error)
	projectByID(ctx context.Context, userID, projectID string) (deliverycore.ProjectRecord, error)
	authorizeProjectWrite(ctx context.Context, userID, projectID string) error
	ServiceByID(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, error)
	ListServices(ctx context.Context, userID, environmentID string) ([]deliverycore.ServiceRecord, error)
	createScheduledVolume(ctx context.Context, userID, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	listVolumes(ctx context.Context, userID, environmentID string) ([]deliverycore.VolumeRecord, error)
	deleteVolume(ctx context.Context, userID, volumeID string) error
	ServiceStatus(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error)
	ListServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error)
	ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error)
	ListAgents(ctx context.Context) ([]deliverycore.AgentRecord, error)
}

type platformDelivery interface {
	DuplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error)

	ReleaseEnvironment(ctx context.Context, environmentID string) ([]deliverycore.ReleasedService, error)
	ApplyDeploymentAction(ctx context.Context, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.DeploymentActionResult, error)
	CreateScheduledService(ctx context.Context, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error)
	UpdateService(ctx context.Context, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error)
	DiscardServiceChanges(ctx context.Context, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error)
	DeleteService(ctx context.Context, serviceID string) error
	ScaleService(ctx context.Context, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error)
	LiveAllocationsByEnvironment(environmentID string) (map[string][]deliverycore.AllocationRecord, error)
	livePositionReader
}

type environmentStore interface {
	listEnvironments(ctx context.Context, userID, projectID string) ([]deliverycore.EnvironmentRecord, error)
	EnvironmentByID(ctx context.Context, userID, environmentID string) (deliverycore.EnvironmentRecord, error)
	createEnvironment(ctx context.Context, userID, projectID, name string) (deliverycore.EnvironmentRecord, error)
	renameEnvironment(ctx context.Context, userID, environmentID, name string) (deliverycore.EnvironmentRecord, error)
	deleteEnvironment(ctx context.Context, userID, environmentID string) ([]string, error)
}

func (s *PlatformService) protoServiceStatus(rec deliverycore.ServiceRecord, allocations []deliverycore.AllocationRecord, index int64) *platformv1.ServiceStatus {
	out := toProtoServiceStatus(rec, allocations, index)
	out.Live = liveReadMeta(s.delivery)
	return out
}

func (s *PlatformService) environmentForUser(ctx context.Context, userID, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.store.EnvironmentByID(ctx, userID, environmentID)
}

type serviceLogStore interface {
	ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) ([]logs.ServiceLog, error)
}

type PlatformServiceOption func(*PlatformService)

func WithDomainCNAMEResolver(resolver routing.Resolver) PlatformServiceOption {
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

func WithServiceLogEmitter(emitter *logs.LogEmitter) PlatformServiceOption {
	return func(service *PlatformService) {
		service.emitter = emitter
	}
}

func WithGitHubSourceInspection(catalog *source.GitHubCatalog, client *source.GitHubClient) PlatformServiceOption {
	return func(service *PlatformService) {
		service.inspector = source.NewInspector(catalog, client)
	}
}

func WithPlatformEvents(events *PlatformEvents) PlatformServiceOption {
	return func(service *PlatformService) {
		service.events = events
	}
}

// WithPlatformLiveOwner gates stateful platform RPCs behind singleton ownership.
// A non-owner answers with a redirect to the live owner instead of serving
// owner-local live state as if it were authoritative.
func WithPlatformLiveOwner(owner LiveOwner) PlatformServiceOption {
	return func(service *PlatformService) {
		service.liveOwner = owner
	}
}

func (s *PlatformService) requireLiveOwner(ctx context.Context) error {
	if s == nil || s.liveOwner == nil {
		return nil
	}
	held, ownerAddr, err := s.liveOwner.Lookup(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "lookup live owner: %v", err)
	}
	if held {
		return nil
	}
	if strings.TrimSpace(ownerAddr) != "" {
		return status.Error(codes.FailedPrecondition, deliverycore.LiveOwnerRedirectMessage(ownerAddr))
	}
	return status.Error(codes.Unavailable, "live owner is not ready")
}

func NewPlatformService(store platformStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, delivery platformDelivery, opts ...PlatformServiceOption) *PlatformService {
	service := &PlatformService{store: store, delivery: delivery, notifier: notifier, ingress: ingress, dnsResolver: routing.NewPublicDNSResolver()}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	service.domains = routing.NewDomains(store, notifier, ingress, service.platformDomainSuffix, service.dnsResolver)
	service.environments = NewEnvironmentOperations(store, notifier, ingress)
	return service
}

func (s *PlatformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.createProject(ctx, identity.UserID, req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "create project: %v", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) ListProjects(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListProjectsResponse, error) {
	identity, err := identity.DelegatedUserFromContext(ctx)
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
	identity, err := identity.DelegatedUserFromContext(ctx)
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
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listEnvironments(ctx, identity.UserID, req.GetProjectId())
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
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.EnvironmentByID(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) CreateEnvironment(ctx context.Context, req *platformv1.CreateEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.createEnvironment(ctx, identity.UserID, req.GetProjectId(), req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "create environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) DuplicateEnvironment(ctx context.Context, req *platformv1.DuplicateEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.delivery.DuplicateEnvironment(ctx, identity.UserID, req.GetSourceEnvironmentId(), req.GetName(), req.GetCopyVariables())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "duplicate environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) RenameEnvironment(ctx context.Context, req *platformv1.RenameEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.renameEnvironment(ctx, identity.UserID, req.GetEnvironmentId(), req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "rename environment: %v", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if _, err := identity.DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	result, err := s.environments.DeleteEnvironment(ctx, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) ReleaseEnvironment(ctx context.Context, req *platformv1.ReleaseEnvironmentRequest) (*platformv1.ReleaseEnvironmentResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	services, err := s.delivery.ReleaseEnvironment(ctx, req.GetEnvironmentId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &platformv1.ReleaseEnvironmentResponse{Services: make([]*platformv1.ServiceStatus, 0, len(services))}
	for _, released := range services {
		resp.Services = append(resp.Services, s.protoServiceStatus(released.Service, released.Allocations, 0))
	}
	return resp, nil
}
