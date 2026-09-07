package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Domains owns domain mutation policy and all work required after its commit.
// The persistence methods commit authorization, binding changes, desired
// revisions, and the durable event revision together. Agent and ingress wakes
// are hints emitted only after that commit.
type Domains struct {
	store                domainStore
	notifier             deliverycore.PlatformNotifier
	ingress              deliverycore.PlatformIngress
	platformDomainSuffix string
	dnsResolver          domainCNAMEResolver
}

type domainStore interface {
	createDomainBinding(context.Context, string, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	createPlatformDomainBinding(context.Context, string, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	updateDomainBinding(context.Context, string, string, string, int32) (deliverycore.DomainBindingRecord, bool, error)
	deleteDomainBinding(context.Context, string, string) (bool, error)
	domainBindingByHostname(context.Context, string, string) (deliverycore.DomainBindingRecord, error)
	platformDomainBindingForService(context.Context, string, string) (deliverycore.DomainBindingRecord, error)
	serviceByID(context.Context, string, string) (deliverycore.ServiceRecord, error)
	listAllocationsByServiceID(context.Context, string) ([]deliverycore.AllocationRecord, error)
	listAgents(context.Context) ([]deliverycore.AgentRecord, error)
}

func NewDomains(store domainStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, suffix string, resolver domainCNAMEResolver) *Domains {
	return &Domains{store: store, notifier: notifier, ingress: ingress, platformDomainSuffix: suffix, dnsResolver: resolver}
}

func (s *Domains) CreateDomainBinding(ctx context.Context, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
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

	binding, changed, err := s.store.createDomainBinding(ctx, identity.UserID, hostname, req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, errPlatformDomainInUse) || errors.Is(err, errPlatformDomainReassignment) || errors.Is(err, errPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding), nil
}

func (s *Domains) GenerateDomainBinding(ctx context.Context, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetPort := req.GetTargetPort()
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := generatedPlatformHostname(service.ProjectID, req.GetServiceId(), s.platformDomainSuffix)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "generate platform hostname: %v", err)
	}
	binding, changed, err := s.store.createPlatformDomainBinding(ctx, identity.UserID, hostname, req.GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, errPlatformDomainInUse) || errors.Is(err, errPlatformDomainReassignment) || errors.Is(err, errPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "generate domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "generate domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding), nil
}

func (s *Domains) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	var previousServiceID string
	if previous, err := s.store.domainBindingByHostname(ctx, identity.UserID, req.GetHostname()); err == nil {
		previousServiceID = previous.ServiceID
	}
	binding, changed, err := s.store.updateDomainBinding(ctx, identity.UserID, req.GetHostname(), req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, errPlatformDomainInUse) || errors.Is(err, errPlatformDomainReassignment) || errors.Is(err, errPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, previousServiceID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding), nil
}

func (s *Domains) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, req.GetHostname())
	if err != nil {
		if errors.Is(err, errPlatformDomainInUse) || errors.Is(err, errPlatformDomainReassignment) || errors.Is(err, errPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}

	changed, err := s.store.deleteDomainBinding(ctx, identity.UserID, req.GetHostname())
	if err != nil {
		if errors.Is(err, errPlatformDomainInUse) || errors.Is(err, errPlatformDomainReassignment) || errors.Is(err, errPlatformDomainNotGenerated) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain binding: %v", err)
	}

	if changed {
		s.notifyServices(ctx, identity.UserID, binding.ServiceID)
		s.ingress.RequestSync()
	}
	return &emptypb.Empty{}, nil
}

func (s *Domains) notifyServices(ctx context.Context, userID string, serviceIDs ...string) {
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
		service, err := s.store.serviceByID(ctx, userID, serviceID)
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
	allocations, err := s.store.listAllocationsByServiceID(ctx, serviceID)
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
	agents, err := s.store.listAgents(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list agents for domain notification", "error", err)
		return
	}
	for _, agent := range agents {
		s.notifier.Notify(agent.ID)
	}
}
