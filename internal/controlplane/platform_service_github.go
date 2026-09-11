package controlplane

import (
	"context"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *PlatformService) LinkGitHubRepository(ctx context.Context, req *platformv1.LinkGitHubRepositoryRequest) (*platformv1.InspectSourceResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
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
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if authErr := gitHubUserAuthorizationStatus(err); authErr != nil {
			return nil, authErr
		}
		return nil, status.Errorf(codes.Internal, "link github repository: %v", err)
	}
	return resp, nil
}

func (s *PlatformService) InspectSource(ctx context.Context, req *platformv1.InspectSourceRequest) (*platformv1.InspectSourceResponse, error) {
	identity, err := identity.DelegatedUserFromContext(ctx)
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
