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
	store     *Store
	scheduler *Scheduler
	notifier  *Notifier
	ingress   *IngressSyncer
}

func NewPlatformService(store *Store, scheduler *Scheduler, notifier *Notifier, ingress *IngressSyncer) *PlatformService {
	return &PlatformService{store: store, scheduler: scheduler, notifier: notifier, ingress: ingress}
}

func (s *PlatformService) EnsurePrincipal(ctx context.Context, req *platformv1.EnsurePrincipalRequest) (*platformv1.Principal, error) {
	if strings.TrimSpace(req.GetSubject()) == "" || strings.TrimSpace(req.GetEmail()) == "" {
		return nil, status.Error(codes.InvalidArgument, "subject and email are required")
	}
	user, err := s.store.ensurePrincipal(ctx, req.GetSubject(), req.GetEmail())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ensure principal: %v", err)
	}
	return toProtoPrincipal(user), nil
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

func (s *PlatformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.createScheduledService(ctx, identity.Subject, req.GetProjectId(), req.GetName(), req.GetSpec(), req.GetDomains())
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) || errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create service: %v", err)
	}
	slog.Info("service created", "service_id", service.ID, "agent_id", service.AllocatedAgentID, "project_id", req.GetProjectId(), "revision", service.CurrentRevision)
	s.notifier.Notify(service.AllocatedAgentID)
	_ = s.ingress.Sync(ctx)
	return toProtoService(service), nil
}

func (s *PlatformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.updateService(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId(), req.GetSpec(), req.GetDomains())
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
	s.notifier.Notify(service.AllocatedAgentID)
	_ = s.ingress.Sync(ctx)
	return toProtoService(service), nil
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
	if err := s.store.deleteService(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete service: %v", err)
	}
	s.notifier.Notify(service.AllocatedAgentID)
	_ = s.ingress.Sync(ctx)
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

func (s *PlatformService) UpsertDomain(ctx context.Context, req *platformv1.UpsertDomainRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.upsertDomain(ctx, identity.Subject, req.GetProjectId(), req.GetServiceId(), req.GetDomain())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "upsert domain: %v", err)
	}
	s.notifier.Notify(service.AllocatedAgentID)
	_ = s.ingress.Sync(ctx)
	return toProtoService(service), nil
}

func (s *PlatformService) DeleteDomain(ctx context.Context, req *platformv1.DeleteDomainRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteDomain(ctx, identity.Subject, req.GetProjectId(), req.GetDomain()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain: %v", err)
	}
	_ = s.ingress.Sync(ctx)
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
	return &platformv1.ServiceStatus{Service: toProtoService(service), Allocation: toProtoAllocation(allocation)}, nil
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
