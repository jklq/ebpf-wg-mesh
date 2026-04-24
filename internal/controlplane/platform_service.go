package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type PlatformService struct {
	platformv1.UnimplementedPlatformServiceServer
	store     platformStore
	logStore  serviceLogStore
	emitter   *LogEmitter
	notifier  platformNotifier
	ingress   platformIngress
	inspector *gitHubSourceInspector
}

type platformStore interface {
	ensurePrincipal(ctx context.Context, subject, email string) (principalRecord, error)
	createProject(ctx context.Context, subject, name string) (projectRecord, error)
	listProjects(ctx context.Context, subject string) ([]projectRecord, error)
	projectByID(ctx context.Context, subject, projectID string) (projectRecord, error)
	createScheduledService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error)
	updateService(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error)
	redeployService(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	requestServiceSourceSync(ctx context.Context, subject, projectID, serviceID string) error
	enqueueBuildForService(ctx context.Context, subject, projectID, serviceID, commitSHA string) (buildRunRecord, error)
	deleteService(ctx context.Context, subject, projectID, serviceID string) error
	serviceByID(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error)
	listServices(ctx context.Context, subject, projectID string) ([]serviceRecord, error)
	createScheduledVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error)
	listVolumes(ctx context.Context, subject, projectID string) ([]volumeRecord, error)
	deleteVolume(ctx context.Context, subject, projectID, volumeID string) error
	createDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	updateDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error)
	domainBindingByHostname(ctx context.Context, subject, projectID, hostname string) (domainBindingRecord, error)
	listDomainBindings(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error)
	deleteDomainBinding(ctx context.Context, subject, projectID, hostname string) (bool, error)
	serviceStatus(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error)
	listServiceDeployments(ctx context.Context, subject, projectID, serviceID string, limit int32) ([]deploymentRecord, error)
	allocationByServiceID(ctx context.Context, serviceID string) (allocationRecord, error)
	listAgents(ctx context.Context) ([]agentRecord, error)
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

func NewPlatformService(store platformStore, notifier platformNotifier, ingress platformIngress, opts ...PlatformServiceOption) *PlatformService {
	service := &PlatformService{store: store, notifier: notifier, ingress: ingress}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *PlatformService) EnsurePrincipal(ctx context.Context, req *platformv1.EnsurePrincipalRequest) (*platformv1.Principal, error) {
	if strings.TrimSpace(req.GetSubject()) == "" || strings.TrimSpace(req.GetEmail()) == "" {
		return nil, status.Error(codes.InvalidArgument, "subject and email are required")
	}
	principal, err := s.store.ensurePrincipal(ctx, req.GetSubject(), req.GetEmail())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ensure principal: %v", err)
	}
	return toProtoPrincipal(principal), nil
}

func (s *PlatformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.createProject(ctx, identity.Subject, req.GetName())
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
	items, err := s.store.listProjects(ctx, identity.Subject)
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
	project, err := s.store.projectByID(ctx, identity.Subject, req.GetProjectId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "project: %v", err)
	}
	return toProtoProject(project), nil
}

func (s *PlatformService) InspectSource(ctx context.Context, req *platformv1.InspectSourceRequest) (*platformv1.InspectSourceResponse, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(strings.ToLower(req.GetProvider())) != "github" {
		return nil, status.Error(codes.InvalidArgument, "unsupported source provider")
	}
	if strings.TrimSpace(req.GetRepositorySelector()) == "" {
		return nil, status.Error(codes.InvalidArgument, "repository selector is required")
	}
	if s.inspector == nil {
		return nil, status.Error(codes.FailedPrecondition, "github source inspection is not configured")
	}
	resp, err := s.inspector.Inspect(ctx, req.GetRepositorySelector())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "inspect source: %v", err)
	}
	return resp, nil
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
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	var service serviceRecord
	service, err = s.store.createScheduledService(ctx, identity.Subject, req.GetProjectId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) || errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create service: %v", err)
	}
	slog.Info("service created", "service_id", service.ID, "agent_id", service.AllocatedAgentID, "project_id", req.GetProjectId(), "spec_revision", service.SpecRevision, "rollout_generation", service.RolloutGeneration)
	s.notifier.Notify(service.AllocatedAgentID)
	service, err = s.store.serviceByID(ctx, identity.Subject, req.GetProjectId(), service.ID)
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
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	var (
		service serviceRecord
		changed bool
	)
	service, changed, err = s.store.updateService(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "update service: %v", err)
		}
		if errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "update service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update service: %v", err)
	}
	if changed {
		s.notifier.Notify(service.AllocatedAgentID)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *PlatformService) RedeployService(ctx context.Context, req *platformv1.RedeployServiceRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	currentService, err := s.store.serviceByID(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if desiredSourceSpec(currentService.Spec) != nil {
		if err := s.store.requestServiceSourceSync(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId()); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "service: %v", err)
			}
			if errors.Is(err, errConcurrentUpdate) {
				return nil, status.Errorf(codes.Aborted, "redeploy service: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "redeploy service: %v", err)
		}
	} else {
		service, err := s.store.redeployService(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "service: %v", err)
			}
			if errors.Is(err, errConcurrentUpdate) {
				return nil, status.Errorf(codes.Aborted, "redeploy service: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "redeploy service: %v", err)
		}
		s.notifier.Notify(service.AllocatedAgentID)
	}
	currentService, allocation, err := s.store.serviceStatus(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "redeploy service status: %v", err)
	}
	currentService, err = s.decorateServiceRecordWithAllocation(ctx, currentService, &allocation)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate redeploy status: %v", err)
	}
	return &platformv1.ServiceStatus{Service: toProtoService(currentService), Allocation: toProtoAllocation(allocation)}, nil
}

func (s *PlatformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	bindings, err := s.store.listDomainBindings(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	if err := s.store.deleteService(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete service: %v", err)
	}
	s.notifier.Notify(service.AllocatedAgentID)
	if len(bindings) > 0 {
		s.ingress.RequestSync()
	}
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
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
	items, err := s.store.listServices(ctx, identity.Subject, req.GetProjectId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list services: %v", err)
	}
	resp := &platformv1.ListServicesResponse{Services: make([]*platformv1.Service, 0, len(items))}
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
	volume, err := s.store.createScheduledVolume(ctx, identity.Subject, req.GetProjectId(), req.GetName(), req.GetSizeBytes())
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "create volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}
	slog.Info("volume created", "volume_id", volume.ID, "agent_id", volume.BoundAgentID, "project_id", req.GetProjectId())
	s.notifier.Notify(volume.BoundAgentID)
	return toProtoVolume(volume), nil
}

func (s *PlatformService) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	volumes, err := s.store.listVolumes(ctx, identity.Subject, req.GetProjectId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	for _, volume := range volumes {
		if volume.ID == req.GetVolumeId() {
			if err := s.store.deleteVolume(ctx, identity.Subject, req.GetProjectId(), req.GetVolumeId()); err != nil {
				if errors.Is(err, errVolumeInUse) {
					return nil, status.Errorf(codes.FailedPrecondition, "delete volume: %v", err)
				}
				if errors.Is(err, sql.ErrNoRows) {
					return nil, status.Errorf(codes.NotFound, "volume: %v", err)
				}
				return nil, status.Errorf(codes.Internal, "delete volume: %v", err)
			}
			s.notifier.Notify(volume.BoundAgentID)
			return &emptypb.Empty{}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "volume not found")
}

func (s *PlatformService) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listVolumes(ctx, identity.Subject, req.GetProjectId())
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
	targetPort := req.GetBinding().GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	binding, changed, err := s.store.createDomainBinding(ctx, identity.Subject, req.GetProjectId(), req.GetBinding().GetHostname(), req.GetBinding().GetServiceId(), targetPort)
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
		s.notifyServices(ctx, identity.Subject, req.GetProjectId(), binding.ServiceID)
		s.ingress.RequestSync()
	}
	return toProtoDomainBinding(binding), nil
}

func (s *PlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.Subject, req.GetProjectId(), req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	return toProtoDomainBinding(binding), nil
}

func (s *PlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listDomainBindings(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	resp := &platformv1.ListDomainBindingsResponse{Bindings: make([]*platformv1.DomainBinding, 0, len(items))}
	for _, item := range items {
		resp.Bindings = append(resp.Bindings, toProtoDomainBinding(item))
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
	if previous, err := s.store.domainBindingByHostname(ctx, identity.Subject, req.GetProjectId(), req.GetHostname()); err == nil {
		previousServiceID = previous.ServiceID
	}
	binding, changed, err := s.store.updateDomainBinding(ctx, identity.Subject, req.GetProjectId(), req.GetHostname(), req.GetBinding().GetServiceId(), targetPort)
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
		s.notifyServices(ctx, identity.Subject, req.GetProjectId(), previousServiceID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return toProtoDomainBinding(binding), nil
}

func (s *PlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var previousServiceID string
	if previous, err := s.store.domainBindingByHostname(ctx, identity.Subject, req.GetProjectId(), req.GetHostname()); err == nil {
		previousServiceID = previous.ServiceID
	}
	changed, err := s.store.deleteDomainBinding(ctx, identity.Subject, req.GetProjectId(), req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.Subject, req.GetProjectId(), previousServiceID)
		s.ingress.RequestSync()
	}
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, allocation, err := s.store.serviceStatus(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service status: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocation(ctx, service, &allocation)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service status: %v", err)
	}
	return &platformv1.ServiceStatus{Service: toProtoService(service), Allocation: toProtoAllocation(allocation)}, nil
}

func (s *PlatformService) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id and service_id are required")
	}
	if s.logStore == nil {
		return nil, status.Error(codes.FailedPrecondition, errLogStoreDisabled.Error())
	}
	if _, err := s.store.serviceByID(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId()); err != nil {
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
	if strings.TrimSpace(req.GetProjectId()) == "" || strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id and service_id are required")
	}
	items, err := s.store.listServiceDeployments(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId(), req.GetLimit())
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
	return s.decorateServiceRecordWithAllocation(ctx, service, nil)
}

func (s *PlatformService) decorateServiceRecordWithAllocation(ctx context.Context, service serviceRecord, alloc *allocationRecord) (serviceRecord, error) {
	if service.SourceSummary == nil {
		service.SourceSummary = buildSourceSummary(service.Spec)
	}
	// Stages are projected from the allocation + latest build; if we cannot
	// load the allocation we still return the stages derived from just the
	// service+build so the UI gets something to render (showing a "waiting"
	// deploy stage rather than a hard error).
	allocValue := allocationRecord{}
	if alloc != nil {
		allocValue = *alloc
	} else {
		var err error
		allocValue, err = s.store.allocationByServiceID(ctx, service.ID)
		if err != nil {
			return serviceRecord{}, err
		}
	}
	var buildRec *buildRunRecord
	if service.LatestBuild != nil {
		rec := buildRunRecordFromProto(service.LatestBuild)
		buildRec = &rec
	}
	stages := projectDeploymentStages(service, buildRec, allocValue)
	if service.LatestBuild == nil && len(stages) > 0 {
		// We need a vehicle to carry the stages back to the client. The
		// proto encodes them on BuildStatus today; for services that have
		// never been built we synthesize a minimal BuildStatus so stages
		// still round-trip without leaking a new top-level field.
		service.LatestBuild = &platformv1.BuildStatus{Stages: stages}
	} else if service.LatestBuild != nil {
		service.LatestBuild.Stages = stages
	}
	return service, nil
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

func validateServiceSpecPorts(spec *platformv1.ServiceSpec) error {
	for _, port := range spec.GetRuntime().GetPorts() {
		if err := validatePort(port.GetPort()); err != nil {
			return err
		}
	}
	if check := spec.GetRuntime().GetHealthCheck(); check != nil && check.GetPort() > 0 {
		if err := validatePort(check.GetPort()); err != nil {
			return err
		}
	}
	return nil
}

func (s *PlatformService) notifyServices(ctx context.Context, subject, projectID string, serviceIDs ...string) {
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
		service, err := s.store.serviceByID(ctx, subject, projectID, serviceID)
		if err != nil {
			slog.Warn("failed to load service for domain notification", "service_id", serviceID, "error", err)
			continue
		}
		s.notifier.Notify(service.AllocatedAgentID)
	}
}
