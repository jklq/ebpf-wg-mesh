package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type platformService struct {
	platformv1.UnimplementedPlatformServiceServer
	store                platformStore
	domains              *routing.Domains
	catalog              *catalogOperations
	delivery             platformDelivery
	logStore             serviceLogStore
	emitter              *logs.LogEmitter
	notifier             deliverycore.PlatformNotifier
	ingress              deliverycore.PlatformIngress
	inspector            *source.Inspector
	dnsResolver          routing.Resolver
	certificates         routing.Certificates
	platformDomainSuffix string
	events               *platformEvents
	liveOwner            liveOwner
}

type platformStore interface {
	catalogStore
	routing.Store
	createProject(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error)
	listProjects(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error)
	projectByID(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error)
	updateProjectLogRetention(ctx context.Context, user authz.User, projectID string, retentionDays int32) (deliverycore.ProjectRecord, error)
	ServiceByID(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	createScheduledVolume(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	ListVolumes(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.VolumeRecord, error)
	deleteVolume(ctx context.Context, user authz.User, volumeID, confirmation string) error
	ServiceStatus(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error)
	ListServiceDeployments(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error)
	ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error)
	ListAgents(ctx context.Context, user authz.User) ([]deliverycore.AgentRecord, error)
}

type platformDelivery interface {
	ReadEnvironmentSnapshot(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) (deliverycore.EnvironmentSnapshot, error)
	DuplicateEnvironment(ctx context.Context, user authz.User, sourceEnvironmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error)

	ReleaseEnvironment(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.ReleasedService, error)
	ApplyDeploymentAction(ctx context.Context, user authz.User, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.DeploymentActionResult, error)
	CreateScheduledService(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error)
	UpdateService(ctx context.Context, user authz.User, serviceID, name string, spec *platformv1.ServiceSpec, expectedSpecRevision int64) (deliverycore.ServiceRecord, bool, error)
	DiscardServiceChanges(ctx context.Context, user authz.User, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error)
	DeleteService(ctx context.Context, user authz.User, serviceID string) error
	RestoreService(ctx context.Context, user authz.User, serviceID string) (deliverycore.ServiceRecord, error)
	ScaleService(ctx context.Context, user authz.User, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error)
	LiveAllocationsByEnvironment(environmentID string) (map[string][]deliverycore.AllocationRecord, error)
	BuildAttempts(ctx context.Context, user authz.User, serviceID, buildID string) ([]deliverycore.BuildAttemptRecord, error)
	ListServiceArtifacts(ctx context.Context, user authz.User, serviceID string, limit int32) ([]deliverycore.BuildArtifactRecord, error)
	GrowVolume(ctx context.Context, user authz.User, volumeID string, sizeBytes int64) (deliverycore.VolumeRecord, error)
	RenderVolumeStatus(rec deliverycore.VolumeRecord) deliverycore.VolumeRecord
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
	previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (deletionPreview, error)
	previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (deletionPreview, error)
	previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (deletionPreview, error)
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

func (s *platformService) protoServiceStatus(rec deliverycore.ServiceRecord, allocations []deliverycore.AllocationRecord, index int64) *platformv1.ServiceStatus {
	out := toProtoServiceStatus(rec, allocations, index)
	out.Live = liveReadMeta(s.delivery)
	return out
}

func (s *platformService) environmentForUser(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.store.EnvironmentByID(ctx, user, environmentID)
}

type serviceLogStore interface {
	ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (logs.ServiceLogPage, error)
}

type platformServiceOption func(*platformService)

func withDomainCNAMEResolver(resolver routing.Resolver) platformServiceOption {
	return func(service *platformService) {
		service.dnsResolver = resolver
	}
}

func withCertificates(certs routing.Certificates) platformServiceOption {
	return func(service *platformService) {
		service.certificates = certs
	}
}

func withPlatformDomainSuffix(suffix string) platformServiceOption {
	return func(service *platformService) {
		service.platformDomainSuffix = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(suffix), "."))
	}
}

func withServiceLogs(logStore serviceLogStore) platformServiceOption {
	return func(service *platformService) {
		service.logStore = logStore
	}
}

func withServiceLogEmitter(emitter *logs.LogEmitter) platformServiceOption {
	return func(service *platformService) {
		service.emitter = emitter
	}
}

func withGitHubSourceInspection(catalog *source.GitHubCatalog, client *source.GitHubClient, authorizer *authz.Authorizer) platformServiceOption {
	return func(service *platformService) {
		service.inspector = source.NewInspector(catalog, client, authorizer)
	}
}

func withPlatformEvents(events *platformEvents) platformServiceOption {
	return func(service *platformService) {
		service.events = events
	}
}

// withPlatformLiveOwner gates stateful platform RPCs behind singleton ownership.
// A non-owner answers with a redirect to the live owner instead of serving
// owner-local live state as if it were authoritative.
func withPlatformLiveOwner(owner liveOwner) platformServiceOption {
	return func(service *platformService) {
		service.liveOwner = owner
	}
}

func (s *platformService) requireLiveOwner(ctx context.Context) error {
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

func newPlatformService(store platformStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, delivery platformDelivery, opts ...platformServiceOption) *platformService {
	service := &platformService{store: store, delivery: delivery, notifier: notifier, ingress: ingress, dnsResolver: routing.NewPublicDNSResolver()}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	service.domains = routing.NewDomains(store, notifier, ingress, service.certificates, service.platformDomainSuffix, service.dnsResolver)
	service.catalog = newCatalogOperations(store, notifier, ingress)
	return service
}

func (s *platformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
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

func (s *platformService) ListProjects(ctx context.Context, req *platformv1.ListProjectsRequest) (*platformv1.ListProjectsResponse, error) {
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

func (s *platformService) DeleteProject(ctx context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
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

func (s *platformService) RestoreProject(ctx context.Context, req *platformv1.RestoreProjectRequest) (*platformv1.Project, error) {
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

func (s *platformService) UpdateProjectLogRetention(ctx context.Context, req *platformv1.UpdateProjectLogRetentionRequest) (*platformv1.Project, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	rec, err := s.store.updateProjectLogRetention(ctx, user, req.GetProjectId(), req.GetLogRetentionDays())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrProjectDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "update project log retention: %v", err)
		}
		if errors.Is(err, errInvalidLogRetention) {
			return nil, status.Errorf(codes.InvalidArgument, "update project log retention: %v", err)
		}
		return nil, writeAccessError("update project log retention", err)
	}
	return toProtoProject(rec), nil
}

func (s *platformService) PreviewProjectDeletion(ctx context.Context, req *platformv1.PreviewProjectDeletionRequest) (*platformv1.DeletionPreview, error) {
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

func (s *platformService) PreviewEnvironmentDeletion(ctx context.Context, req *platformv1.PreviewEnvironmentDeletionRequest) (*platformv1.DeletionPreview, error) {
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

func (s *platformService) PreviewVolumeDeletion(ctx context.Context, req *platformv1.PreviewVolumeDeletionRequest) (*platformv1.DeletionPreview, error) {
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

func (s *platformService) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
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

func (s *platformService) ListEnvironments(ctx context.Context, req *platformv1.ListEnvironmentsRequest) (*platformv1.ListEnvironmentsResponse, error) {
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

func (s *platformService) GetEnvironment(ctx context.Context, req *platformv1.GetEnvironmentRequest) (*platformv1.Environment, error) {
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

func (s *platformService) CreateEnvironment(ctx context.Context, req *platformv1.CreateEnvironmentRequest) (*platformv1.Environment, error) {
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

func (s *platformService) DuplicateEnvironment(ctx context.Context, req *platformv1.DuplicateEnvironmentRequest) (*platformv1.Environment, error) {
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

func (s *platformService) RenameEnvironment(ctx context.Context, req *platformv1.RenameEnvironmentRequest) (*platformv1.Environment, error) {
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

func (s *platformService) UpdateEnvironmentAutoDeploy(ctx context.Context, req *platformv1.UpdateEnvironmentAutoDeployRequest) (*platformv1.Environment, error) {
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

func (s *platformService) RestoreEnvironment(ctx context.Context, req *platformv1.RestoreEnvironmentRequest) (*platformv1.Environment, error) {
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

func (s *platformService) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	result, err := s.catalog.DeleteEnvironment(ctx, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *platformService) ReleaseEnvironment(ctx context.Context, req *platformv1.ReleaseEnvironmentRequest) (*platformv1.ReleaseEnvironmentResponse, error) {
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

func (s *platformService) LinkGitHubRepository(ctx context.Context, req *platformv1.LinkGitHubRepositoryRequest) (*platformv1.InspectSourceResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetRepositorySelector()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project and repository selector are required")
	}
	if s.inspector == nil {
		return nil, status.Error(codes.FailedPrecondition, "github source inspection is not configured")
	}
	resp, err := s.inspector.LinkAndInspect(ctx, user, req.GetProjectId(), req.GetRepositorySelector(), req.GetGithubUserAccessToken())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "project access: %v", err)
		}
		if authErr := gitHubUserAuthorizationStatus(err); authErr != nil {
			return nil, authErr
		}
		return nil, status.Errorf(codes.Internal, "link github repository: %v", err)
	}
	return resp, nil
}

func (s *platformService) InspectSource(ctx context.Context, req *platformv1.InspectSourceRequest) (*platformv1.InspectSourceResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(strings.ToLower(req.GetProvider())) != "github" {
		return nil, status.Error(codes.InvalidArgument, "unsupported source provider")
	}
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetRepositorySelector()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project and repository selector are required")
	}
	if s.inspector == nil {
		return nil, status.Error(codes.FailedPrecondition, "github source inspection is not configured")
	}
	resp, err := s.inspector.Inspect(ctx, user, req.GetProjectId(), req.GetRepositorySelector(), req.GetGithubUserAccessToken())
	if err != nil {
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "project access: %v", err)
		}
		if authErr := gitHubUserAuthorizationStatus(err); authErr != nil {
			return nil, authErr
		}
		return nil, status.Errorf(codes.Internal, "inspect source: %v", err)
	}
	return resp, nil
}

func gitHubUserAuthorizationStatus(err error) error {
	var authErr *source.GitHubUserRepositoryAuthorizationError
	if !errors.As(err, &authErr) {
		return nil
	}
	if errors.Is(authErr, source.ErrGitHubUserAccessTokenRequired) {
		return status.Error(codes.Unauthenticated, "GitHub user authorization is required")
	}
	var apiErr *source.GitHubAPIError
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
	if errors.Is(authErr, source.ErrGitHubRepositoryIdentityMismatch) {
		return status.Error(codes.PermissionDenied, "GitHub repository identity could not be verified")
	}
	return status.Error(codes.Unavailable, "GitHub user authorization is temporarily unavailable")
}

func (s *platformService) authorizeServiceSource(ctx context.Context, projectID string, spec *platformv1.ServiceSpec) error {
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
