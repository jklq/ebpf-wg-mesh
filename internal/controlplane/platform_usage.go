package controlplane

import (
	"context"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/usage"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func withUsage(store *usage.Store, authorizer *authz.Authorizer) platformServiceOption {
	return func(service *platformService) {
		service.usage = store
		service.authz = authorizer
	}
}

// GetProjectUsage reads a project's usage with the project's read access.
// Retained attribution grants nothing: once a project's catalog row is
// gone, only a platform operator can read its history.
func (s *platformService) GetProjectUsage(ctx context.Context, req *platformv1.GetProjectUsageRequest) (*platformv1.ProjectUsage, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(req.GetProjectId())
	if projectID == "" || !req.GetStartTime().IsValid() || !req.GetEndTime().IsValid() {
		return nil, status.Error(codes.InvalidArgument, "project_id, start_time, and end_time are required")
	}
	if s.usage == nil || s.authz == nil {
		return nil, status.Error(codes.FailedPrecondition, "usage is not configured")
	}
	access, err := s.usageAccess(ctx, user, projectID)
	if err != nil {
		return nil, err
	}
	result, err := s.usage.ProjectUsage(ctx, access, usage.Query{
		Start: req.GetStartTime().AsTime(), End: req.GetEndTime().AsTime(), Resolution: req.GetResolution(),
		EnvironmentID: req.GetEnvironmentId(), ServiceID: req.GetServiceId(),
		AllocationID: req.GetAllocationId(), VolumeID: req.GetVolumeId(),
		ResourceBuckets: req.GetResourceBuckets(),
	})
	if errors.Is(err, usage.ErrInvalidQuery) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "project usage: %v", err)
	}
	return result, nil
}

func (s *platformService) usageAccess(ctx context.Context, user authz.User, projectID string) (usage.Access, error) {
	project, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err == nil {
		return usage.ProjectAccess(project), nil
	}
	if !errors.Is(err, authz.ErrDenied) {
		return usage.Access{}, status.Errorf(codes.Internal, "authorize project usage: %v", err)
	}
	exists, err := s.usage.ProjectExists(ctx, projectID)
	if err != nil {
		return usage.Access{}, status.Errorf(codes.Internal, "authorize project usage: %v", err)
	}
	if !exists {
		if operator, err := s.authz.AuthorizeOperator(ctx, user); err == nil {
			return usage.DeletedProjectAccess(operator, projectID), nil
		}
	}
	return usage.Access{}, status.Error(codes.NotFound, "project usage: project not found")
}
