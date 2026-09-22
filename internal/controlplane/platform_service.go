package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type PlatformService struct {
	platformv1.UnimplementedPlatformServiceServer
	store                platformStore
	domains              *routing.Domains
	catalog              *CatalogOperations
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
	catalogStore
	routing.Store
	createProject(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error)
	listProjects(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error)
	projectByID(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	ServiceByID(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	ListServices(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.ServiceRecord, error)
	createScheduledVolume(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	listVolumes(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.VolumeRecord, error)
	deleteVolume(ctx context.Context, user authz.User, volumeID, confirmation string) error
	ServiceStatus(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error)
	ListServiceDeployments(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error)
	ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error)
	ListAgents(ctx context.Context, user authz.User) ([]deliverycore.AgentRecord, error)
}

type platformDelivery interface {
	DuplicateEnvironment(ctx context.Context, user authz.User, sourceEnvironmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error)

	ReleaseEnvironment(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ReleasedService, error)
	ApplyDeploymentAction(ctx context.Context, user authz.User, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.DeploymentActionResult, error)
	CreateScheduledService(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error)
	UpdateService(ctx context.Context, user authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error)
	DiscardServiceChanges(ctx context.Context, user authz.User, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error)
	DeleteService(ctx context.Context, user authz.User, serviceID string) error
	RestoreService(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	ScaleService(ctx context.Context, user authz.User, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error)
	LiveAllocationsByEnvironment(environmentID string) (map[string][]deliverycore.AllocationRecord, error)
	SealServiceSecret(ctx context.Context, user authz.User, serviceID, name string, value []byte) (int64, error)
	DeleteServiceSecret(ctx context.Context, user authz.User, serviceID, name string) error
	ListServiceSecrets(ctx context.Context, user authz.User, serviceID string) ([]secretkeys.SecretMetadata, error)
	BuildAttempts(ctx context.Context, user authz.User, serviceID, buildID string) ([]deliverycore.BuildAttemptRecord, error)
	ListServiceArtifacts(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.BuildArtifactRecord, error)
	livePositionReader
}

type catalogStore interface {
	listEnvironments(ctx context.Context, user authz.User, projectID string, includeDeleted bool) ([]deliverycore.EnvironmentRecord, error)
	EnvironmentByID(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error)
	createEnvironment(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error)
	renameEnvironment(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error)
	updateEnvironmentAutoDeploy(ctx context.Context, user authz.User, environmentID string, autoDeploy bool) (deliverycore.EnvironmentRecord, error)
	deleteEnvironment(ctx context.Context, user authz.User, environmentID, confirmation string) ([]string, error)
	restoreEnvironment(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error)
	deleteProject(ctx context.Context, user authz.User, projectID, confirmation string) ([]string, error)
	restoreProject(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (DeletionPreview, error)
	previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (DeletionPreview, error)
	previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (DeletionPreview, error)
	AgentIDs(ctx context.Context) ([]string, error)
}

// authorizedUser resolves the delegated dashboard user into the authorization
// identity every store and delivery entry requires.
func authorizedUser(ctx context.Context) (authz.User, error) {
	return identity.UserFromContext(ctx)
}

// writeAccessError maps a write or list failure to its RPC code. Denials are
// PermissionDenied, genuinely missing rows are NotFound, and anything else is
// Internal. ErrDenied wraps sql.ErrNoRows by design, so it must be checked
// first.
func writeAccessError(op string, err error) error {
	switch {
	case errors.Is(err, authz.ErrDenied):
		return status.Errorf(codes.PermissionDenied, "%s: %v", op, err)
	case errors.Is(err, sql.ErrNoRows):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// readAccessError maps a single-resource read failure to its RPC code. Reads
// stay indistinguishable: denials and missing rows are both NotFound.
func readAccessError(op string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}

func (s *PlatformService) protoServiceStatus(rec deliverycore.ServiceRecord, allocations []deliverycore.AllocationRecord, index int64) *platformv1.ServiceStatus {
	out := toProtoServiceStatus(rec, allocations, index)
	out.Live = liveReadMeta(s.delivery)
	return out
}

func (s *PlatformService) environmentForUser(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.store.EnvironmentByID(ctx, user, environmentID)
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

func WithGitHubSourceInspection(catalog *source.GitHubCatalog, client *source.GitHubClient, authorizer *authz.Authorizer) PlatformServiceOption {
	return func(service *PlatformService) {
		service.inspector = source.NewInspector(catalog, client, authorizer)
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
	service.catalog = NewCatalogOperations(store, notifier, ingress)
	return service
}

func (s *PlatformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.createProject(ctx, user, req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrProjectDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "create project: %v", err)
		}
		return nil, writeAccessError("create project", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) ListProjects(ctx context.Context, req *platformv1.ListProjectsRequest) (*platformv1.ListProjectsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listProjects(ctx, user, req.GetIncludeDeleted())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list projects: %v", err)
	}
	resp := &platformv1.ListProjectsResponse{Projects: make([]*platformv1.Project, 0, len(items))}
	for _, item := range items {
		resp.Projects = append(resp.Projects, toProtoProject(item))
	}
	return resp, nil
}

func (s *PlatformService) DeleteProject(ctx context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	result, err := s.catalog.DeleteProject(ctx, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) RestoreProject(ctx context.Context, req *platformv1.RestoreProjectRequest) (*platformv1.Project, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	result, err := s.catalog.RestoreProject(ctx, req.GetProjectId())
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) PreviewProjectDeletion(ctx context.Context, req *platformv1.PreviewProjectDeletionRequest) (*platformv1.DeletionPreview, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	preview, err := s.store.previewProjectDeletion(ctx, user, req.GetProjectId())
	if err != nil {
		return nil, writeAccessError("preview project deletion", err)
	}
	return toProtoDeletionPreview(preview), nil
}

func (s *PlatformService) PreviewEnvironmentDeletion(ctx context.Context, req *platformv1.PreviewEnvironmentDeletionRequest) (*platformv1.DeletionPreview, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetEnvironmentId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "environment_id is required")
	}
	preview, err := s.store.previewEnvironmentDeletion(ctx, user, req.GetEnvironmentId())
	if err != nil {
		return nil, writeAccessError("preview environment deletion", err)
	}
	return toProtoDeletionPreview(preview), nil
}

func (s *PlatformService) PreviewVolumeDeletion(ctx context.Context, req *platformv1.PreviewVolumeDeletionRequest) (*platformv1.DeletionPreview, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	preview, err := s.store.previewVolumeDeletion(ctx, user, req.GetVolumeId())
	if err != nil {
		return nil, writeAccessError("preview volume deletion", err)
	}
	return toProtoDeletionPreview(preview), nil
}

func (s *PlatformService) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.projectByID(ctx, user, req.GetProjectId())
	if err != nil {
		return nil, readAccessError("project", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) ListEnvironments(ctx context.Context, req *platformv1.ListEnvironmentsRequest) (*platformv1.ListEnvironmentsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listEnvironments(ctx, user, req.GetProjectId(), req.GetIncludeDeleted())
	if err != nil {
		return nil, writeAccessError("list environments", err)
	}
	resp := &platformv1.ListEnvironmentsResponse{Environments: make([]*platformv1.Environment, 0, len(items))}
	for _, item := range items {
		resp.Environments = append(resp.Environments, toProtoEnvironment(item))
	}
	return resp, nil
}

func (s *PlatformService) GetEnvironment(ctx context.Context, req *platformv1.GetEnvironmentRequest) (*platformv1.Environment, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.EnvironmentByID(ctx, user, req.GetEnvironmentId())
	if err != nil {
		return nil, readAccessError("environment", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) CreateEnvironment(ctx context.Context, req *platformv1.CreateEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.createEnvironment(ctx, user, req.GetProjectId(), req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrEnvironmentAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create environment: %v", err)
		}
		if errors.Is(err, deliverycore.ErrProjectDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "create environment: %v", err)
		}
		return nil, writeAccessError("create environment", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) DuplicateEnvironment(ctx context.Context, req *platformv1.DuplicateEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.delivery.DuplicateEnvironment(ctx, user, req.GetSourceEnvironmentId(), req.GetName(), req.GetCopyVariables())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "duplicate environment: %v", err)
		}
		return nil, writeAccessError("duplicate environment", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) RenameEnvironment(ctx context.Context, req *platformv1.RenameEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.renameEnvironment(ctx, user, req.GetEnvironmentId(), req.GetName())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "rename environment: %v", err)
		}
		return nil, writeAccessError("rename environment", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) UpdateEnvironmentAutoDeploy(ctx context.Context, req *platformv1.UpdateEnvironmentAutoDeployRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.updateEnvironmentAutoDeploy(ctx, user, req.GetEnvironmentId(), req.GetAutoDeploy())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "update environment auto-deploy: %v", err)
		}
		return nil, writeAccessError("update environment auto-deploy", err)
	}
	return toProtoEnvironment(rec), nil
}

func (s *PlatformService) RestoreEnvironment(ctx context.Context, req *platformv1.RestoreEnvironmentRequest) (*platformv1.Environment, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetEnvironmentId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "environment_id is required")
	}
	result, err := s.catalog.RestoreEnvironment(ctx, req.GetEnvironmentId())
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	result, err := s.catalog.DeleteEnvironment(ctx, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) ReleaseEnvironment(ctx context.Context, req *platformv1.ReleaseEnvironmentRequest) (*platformv1.ReleaseEnvironmentResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	services, err := s.delivery.ReleaseEnvironment(ctx, user, req.GetEnvironmentId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "release environment: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "release environment: %v", err)
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &platformv1.ReleaseEnvironmentResponse{Services: make([]*platformv1.ServiceStatus, 0, len(services))}
	for _, released := range services {
		resp.Services = append(resp.Services, s.protoServiceStatus(released.Service, released.Allocations, 0))
	}
	return resp, nil
}
