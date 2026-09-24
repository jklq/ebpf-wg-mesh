package routing

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Domains struct {
	store                Store
	notifier             deliverycore.PlatformNotifier
	ingress              deliverycore.PlatformIngress
	platformDomainSuffix string
	dnsResolver          Resolver
}

type Store interface {
	CreateDomainBindingRecord(context.Context, authz.User, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	CreatePlatformDomainBindingRecord(context.Context, authz.User, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	UpdateDomainBindingRecord(context.Context, authz.User, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	DeleteDomainBindingRecord(context.Context, authz.User, string) (bool, error)
	RestoreDomainBindingRecord(context.Context, authz.User, string) (deliverycore.DomainBindingRecord, error)
	DomainBindingByHostname(context.Context, authz.User, string) (deliverycore.DomainBindingRecord, error)
	PlatformDomainBindingForService(context.Context, authz.User, string) (deliverycore.DomainBindingRecord, error)
	ServiceByID(context.Context, authz.User, string) (deliverycore.ServiceRecord, error)
	ListAllocationsByServiceID(context.Context, string) ([]deliverycore.AllocationRecord, error)
	AgentIDs(context.Context) ([]string, error)
	ListDomainBindings(context.Context, authz.User, string, bool) ([]deliverycore.DomainBindingRecord, error)
}

func NewDomains(store Store, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, suffix string, resolver Resolver) *Domains {
	return &Domains{store: store, notifier: notifier, ingress: ingress, platformDomainSuffix: suffix, dnsResolver: resolver}
}

func (s *Domains) GetDomainBinding(ctx context.Context, user authz.User, hostname string) (*platformv1.DomainBinding, error) {
	rec, err := s.store.DomainBindingByHostname(ctx, user, hostname)
	if err != nil {
		return nil, err
	}
	return s.AnnotateDomainBinding(ctx, user, rec), nil
}

func (s *Domains) ListDomainBindings(ctx context.Context, user authz.User, serviceID string, includeDeleted bool) ([]*platformv1.DomainBinding, error) {
	records, err := s.store.ListDomainBindings(ctx, user, serviceID, includeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]*platformv1.DomainBinding, 0, len(records))
	for _, rec := range records {
		out = append(out, s.AnnotateDomainBinding(ctx, user, rec))
	}
	return out, nil
}

func (s *Domains) CreateDomainBinding(ctx context.Context, user authz.User, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	hostname, err := canonicalDomainHostname(req.GetBinding().GetHostname())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hostname: %v", err)
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	if isPlatformHostname(hostname, s.platformDomainSuffix) {
		return nil, status.Error(codes.InvalidArgument, "hostname is reserved for generated platform domains")
	}

	binding, changed, err := s.store.CreateDomainBindingRecord(ctx, user, hostname, req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "create domain binding: %v", err)
		}
		if errors.Is(err, ErrPlatformDomainInUse) || errors.Is(err, ErrPlatformDomainReassignment) || errors.Is(err, ErrPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrServiceDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "create domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, user, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.AnnotateDomainBinding(ctx, user, binding), nil
}

func (s *Domains) GenerateDomainBinding(ctx context.Context, user authz.User, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	targetPort := req.GetTargetPort()
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	service, err := s.store.ServiceByID(ctx, user, req.GetServiceId())
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "generate domain binding: %v", err)
		}
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := generatedPlatformHostname(service.ProjectID, req.GetServiceId(), s.platformDomainSuffix)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "generate platform hostname: %v", err)
	}
	binding, changed, err := s.store.CreatePlatformDomainBindingRecord(ctx, user, hostname, req.GetServiceId(), targetPort)
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "generate domain binding: %v", err)
		}
		if errors.Is(err, ErrPlatformDomainInUse) || errors.Is(err, ErrPlatformDomainReassignment) || errors.Is(err, ErrPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "generate domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrServiceDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "generate domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "generate domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, user, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.AnnotateDomainBinding(ctx, user, binding), nil
}

func (s *Domains) UpdateDomainBinding(ctx context.Context, user authz.User, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	var previousServiceID string
	if previous, err := s.store.DomainBindingByHostname(ctx, user, req.GetHostname()); ownershipError(err) {
		return nil, err
	} else if err == nil {
		previousServiceID = previous.ServiceID
	}
	binding, changed, err := s.store.UpdateDomainBindingRecord(ctx, user, req.GetHostname(), req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "update domain binding: %v", err)
		}
		if errors.Is(err, ErrPlatformDomainInUse) || errors.Is(err, ErrPlatformDomainReassignment) || errors.Is(err, ErrPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrServiceDeleted) || errors.Is(err, deliverycore.ErrDomainDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "update domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, user, previousServiceID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.AnnotateDomainBinding(ctx, user, binding), nil
}

func (s *Domains) DeleteDomainBinding(ctx context.Context, user authz.User, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	binding, err := s.store.DomainBindingByHostname(ctx, user, req.GetHostname())
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "delete domain binding: %v", err)
		}
		if errors.Is(err, ErrPlatformDomainInUse) || errors.Is(err, ErrPlatformDomainReassignment) || errors.Is(err, ErrPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}

	changed, err := s.store.DeleteDomainBindingRecord(ctx, user, req.GetHostname())
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "delete domain binding: %v", err)
		}
		if errors.Is(err, ErrPlatformDomainInUse) || errors.Is(err, ErrPlatformDomainReassignment) || errors.Is(err, ErrPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain binding: %v", err)
	}

	if changed {
		s.notifyServices(ctx, user, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return &emptypb.Empty{}, nil
}

func (s *Domains) RestoreDomainBinding(ctx context.Context, user authz.User, hostname string) (*platformv1.DomainBinding, error) {
	binding, err := s.store.RestoreDomainBindingRecord(ctx, user, hostname)
	if err != nil {
		if ownershipError(err) {
			return nil, err
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "restore domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrAncestorDeleted) || errors.Is(err, deliverycore.ErrDeletionExpired) {
			return nil, status.Errorf(codes.FailedPrecondition, "restore domain binding: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "restore domain binding: %v", err)
	}
	s.notifyServices(ctx, user, binding.ServiceID)
	s.ingress.RequestSync()
	return s.AnnotateDomainBinding(ctx, user, binding), nil
}

func ownershipError(err error) bool {
	return errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost)
}

func (s *Domains) notifyServices(ctx context.Context, user authz.User, serviceIDs ...string) {
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
		service, err := s.store.ServiceByID(ctx, user, serviceID)
		if err != nil {
			slog.Warn("failed to load service for domain notification", "service_id", serviceID, "error", err)
			continue
		}
		s.notifyServiceAgents(ctx, service.ID)
	}
}

func (s *Domains) notifyServiceAgents(ctx context.Context, serviceID string) {
	if s.notifier == nil {
		return
	}
	allocations, err := s.store.ListAllocationsByServiceID(ctx, serviceID)
	if err == nil && len(allocations) > 0 {
		seen := map[string]bool{}
		for _, allocation := range allocations {
			if allocation.AgentID != "" && !seen[allocation.AgentID] {
				s.notifier.Notify(allocation.AgentID)
				seen[allocation.AgentID] = true
			}
		}
		return
	}
	ids, err := s.store.AgentIDs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list agents for domain notification", "error", err)
		return
	}
	for _, id := range ids {
		s.notifier.Notify(id)
	}
}
