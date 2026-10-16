package controlplane

import (
	"context"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// apiTokenStore is the persistence the API token RPCs need. It is wired
// separately from platformStore so test services without token support
// fail closed in requireAPITokens instead of growing the catalog surface.
type apiTokenStore interface {
	createAPIToken(ctx context.Context, userID, email string, in createAPITokenInput) (apiTokenRecord, string, error)
	listAPITokens(ctx context.Context, where string, arg any, includeRevoked bool) ([]apiTokenRecord, error)
	apiTokenByID(ctx context.Context, tokenID string) (apiTokenRecord, error)
	revokeAPIToken(ctx context.Context, tokenID string) error
}

func withAPITokens(store apiTokenStore, authorizer *authz.Authorizer) platformServiceOption {
	return func(service *platformService) {
		service.apiTokens = store
		if service.authz == nil {
			service.authz = authorizer
		}
	}
}

func (s *platformService) requireAPITokens(op string) error {
	if s == nil || s.apiTokens == nil || s.authz == nil {
		return status.Errorf(codes.FailedPrecondition, "%s: api tokens are not configured", op)
	}
	return nil
}

func apiTokenError(op string, err error) error {
	switch {
	case errors.Is(err, errAPITokenNameRequired), errors.Is(err, errAPITokenNameTooLong),
		errors.Is(err, errAPITokenScope), errors.Is(err, errAPITokenExpiry):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, errAPITokenNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	default:
		return writeAccessError(op, err)
	}
}

func toProtoAPIToken(rec apiTokenRecord) *platformv1.APIToken {
	out := &platformv1.APIToken{
		Id:              rec.ID,
		Name:            rec.Name,
		Prefix:          rec.Prefix,
		WorkspaceId:     rec.WorkspaceID,
		ProjectId:       rec.ProjectID,
		EnvironmentId:   rec.EnvironmentID,
		CreatedByUserId: rec.CreatedByUserID,
		CreatedAt:       ts(rec.CreatedAt),
	}
	switch rec.Scope {
	case apiTokenScopeAccount:
		out.Scope = platformv1.APITokenScope_API_TOKEN_SCOPE_ACCOUNT
	case apiTokenScopeWorkspace:
		out.Scope = platformv1.APITokenScope_API_TOKEN_SCOPE_WORKSPACE
	case apiTokenScopeProject:
		out.Scope = platformv1.APITokenScope_API_TOKEN_SCOPE_PROJECT
	}
	switch rec.Access {
	case apiTokenAccessRead:
		out.Access = platformv1.APITokenAccess_API_TOKEN_ACCESS_READ
	case apiTokenAccessFull:
		out.Access = platformv1.APITokenAccess_API_TOKEN_ACCESS_FULL
	}
	if rec.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*rec.ExpiresAt)
	}
	if rec.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*rec.LastUsedAt)
	}
	if rec.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*rec.RevokedAt)
	}
	return out
}

func (s *platformService) CreateAPIToken(ctx context.Context, req *platformv1.CreateAPITokenRequest) (*platformv1.CreateAPITokenResponse, error) {
	const op = "create api token"
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if err := s.requireAPITokens(op); err != nil {
		return nil, err
	}
	user, email, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	name, err := normalizeAPITokenName(req.GetName())
	if err != nil {
		return nil, apiTokenError(op, err)
	}
	in := createAPITokenInput{Name: name, ExpiresInDays: req.GetExpiresInDays()}
	switch req.GetAccess() {
	case platformv1.APITokenAccess_API_TOKEN_ACCESS_READ:
		in.Access = apiTokenAccessRead
	default:
		in.Access = apiTokenAccessFull
	}
	switch req.GetScope() {
	case platformv1.APITokenScope_API_TOKEN_SCOPE_ACCOUNT:
		in.Scope = apiTokenScopeAccount
	case platformv1.APITokenScope_API_TOKEN_SCOPE_WORKSPACE:
		workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
		if err != nil {
			return nil, err
		}
		// Workspace tokens are shared team credentials: only admins mint them.
		if _, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage); err != nil {
			return nil, apiTokenError(op, err)
		}
		in.Scope, in.WorkspaceID = apiTokenScopeWorkspace, workspaceID
	case platformv1.APITokenScope_API_TOKEN_SCOPE_PROJECT:
		projectID, err := requiredID("project_id", req.GetProjectId())
		if err != nil {
			return nil, err
		}
		environmentID, err := requiredID("environment_id", req.GetEnvironmentId())
		if err != nil {
			return nil, err
		}
		env, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
		if err != nil {
			return nil, apiTokenError(op, err)
		}
		if env.ProjectID() != projectID {
			return nil, apiTokenError(op, errAPITokenScope)
		}
		in.Scope = apiTokenScopeProject
		in.WorkspaceID, in.ProjectID, in.EnvironmentID = env.WorkspaceID(), projectID, environmentID
	default:
		return nil, apiTokenError(op, errAPITokenScope)
	}
	rec, secret, err := s.apiTokens.createAPIToken(ctx, user.ID(), email, in)
	if err != nil {
		return nil, apiTokenError(op, err)
	}
	return &platformv1.CreateAPITokenResponse{Token: toProtoAPIToken(rec), Secret: secret}, nil
}

func (s *platformService) ListAPITokens(ctx context.Context, req *platformv1.ListAPITokensRequest) (*platformv1.ListAPITokensResponse, error) {
	const op = "list api tokens"
	if err := s.requireAPITokens(op); err != nil {
		return nil, err
	}
	user, _, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	var recs []apiTokenRecord
	if workspaceID := strings.TrimSpace(req.GetWorkspaceId()); workspaceID != "" {
		if _, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage); err != nil {
			return nil, apiTokenError(op, err)
		}
		recs, err = s.apiTokens.listAPITokens(ctx, "workspace_id = $1", workspaceID, req.GetIncludeRevoked())
	} else {
		recs, err = s.apiTokens.listAPITokens(ctx, "created_by_user_id = $1", user.ID(), req.GetIncludeRevoked())
	}
	if err != nil {
		return nil, apiTokenError(op, err)
	}
	out := &platformv1.ListAPITokensResponse{Tokens: make([]*platformv1.APIToken, 0, len(recs))}
	for _, rec := range recs {
		out.Tokens = append(out.Tokens, toProtoAPIToken(rec))
	}
	return out, nil
}

func (s *platformService) RevokeAPIToken(ctx context.Context, req *platformv1.RevokeAPITokenRequest) (*emptypb.Empty, error) {
	const op = "revoke api token"
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if err := s.requireAPITokens(op); err != nil {
		return nil, err
	}
	user, _, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	tokenID, err := requiredID("token_id", req.GetTokenId())
	if err != nil {
		return nil, err
	}
	rec, err := s.apiTokens.apiTokenByID(ctx, tokenID)
	if err != nil {
		return nil, apiTokenError(op, err)
	}
	// The creator may always revoke; a workspace admin may revoke tokens
	// scoped to their workspace. Everyone else sees a missing token.
	if rec.CreatedByUserID != user.ID() {
		allowed := false
		if rec.WorkspaceID != "" {
			_, authErr := s.authz.AuthorizeWorkspace(ctx, user, rec.WorkspaceID, authz.Manage)
			allowed = authErr == nil
		}
		if !allowed {
			return nil, apiTokenError(op, errAPITokenNotFound)
		}
	}
	if err := s.apiTokens.revokeAPIToken(ctx, tokenID); err != nil {
		return nil, apiTokenError(op, err)
	}
	return &emptypb.Empty{}, nil
}
