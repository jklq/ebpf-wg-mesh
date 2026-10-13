package controlplane

import (
	"context"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// workspaceError maps workspace failures to RPC codes, falling back to the
// shared access mapping for denials and missing rows.
func workspaceError(op string, err error) error {
	switch {
	case errors.Is(err, errWorkspaceNameRequired),
		errors.Is(err, errWorkspaceNameTooLong),
		errors.Is(err, errInvalidInvitationEmail),
		errors.Is(err, deliverycore.ErrConfirmationMismatch):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, errPersonalWorkspace),
		errors.Is(err, errLastWorkspaceAdmin),
		errors.Is(err, errWorkspaceHasProjects),
		errors.Is(err, deliverycore.ErrProjectDeleted):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, errInvitationNotFound), errors.Is(err, errWorkspaceMemberMissing):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, errProjectNameTaken):
		return status.Errorf(codes.AlreadyExists, "%s: %v", op, err)
	default:
		return writeAccessError(op, err)
	}
}

func requiredID(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s is required", name)
	}
	return value, nil
}

func roleFromProto(role platformv1.WorkspaceRole) (authz.Role, error) {
	switch role {
	case platformv1.WorkspaceRole_WORKSPACE_ROLE_ADMIN:
		return authz.RoleAdmin, nil
	case platformv1.WorkspaceRole_WORKSPACE_ROLE_MEMBER:
		return authz.RoleMember, nil
	case platformv1.WorkspaceRole_WORKSPACE_ROLE_VIEWER:
		return authz.RoleViewer, nil
	default:
		return "", status.Error(codes.InvalidArgument, "role is required")
	}
}

func toProtoWorkspaceRole(role authz.Role) platformv1.WorkspaceRole {
	switch role {
	case authz.RoleAdmin:
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_ADMIN
	case authz.RoleMember:
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_MEMBER
	case authz.RoleViewer:
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_VIEWER
	default:
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_UNSPECIFIED
	}
}

func toProtoWorkspace(rec workspaceRecord) *platformv1.Workspace {
	return &platformv1.Workspace{
		Id:        rec.ID,
		Name:      rec.Name,
		CreatedAt: ts(rec.CreatedAt),
		Personal:  rec.Personal,
		Role:      toProtoWorkspaceRole(rec.Role),
	}
}

func toProtoWorkspaceMember(rec workspaceMemberRecord) *platformv1.WorkspaceMember {
	return &platformv1.WorkspaceMember{
		UserId:   rec.UserID,
		Role:     toProtoWorkspaceRole(rec.Role),
		JoinedAt: ts(rec.JoinedAt),
	}
}

func toProtoWorkspaceInvitations(recs []workspaceInvitationRecord) []*platformv1.WorkspaceInvitation {
	out := make([]*platformv1.WorkspaceInvitation, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toProtoWorkspaceInvitation(rec))
	}
	return out
}

func toProtoWorkspaceInvitation(rec workspaceInvitationRecord) *platformv1.WorkspaceInvitation {
	return &platformv1.WorkspaceInvitation{
		Id:              rec.ID,
		WorkspaceId:     rec.WorkspaceID,
		WorkspaceName:   rec.WorkspaceName,
		Email:           rec.Email,
		Role:            toProtoWorkspaceRole(rec.Role),
		InvitedByUserId: rec.InvitedByUserID,
		CreatedAt:       ts(rec.CreatedAt),
	}
}

// delegatedUserWithEmail resolves the caller and the verified email that
// invitations are addressed to. The email is empty when the dashboard has none.
func delegatedUserWithEmail(ctx context.Context) (authz.User, string, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return authz.User{}, "", err
	}
	delegated, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return authz.User{}, "", err
	}
	return user, delegated.Email, nil
}

func (s *platformService) ListWorkspaces(ctx context.Context, _ *platformv1.ListWorkspacesRequest) (*platformv1.ListWorkspacesResponse, error) {
	user, email, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	workspaces, invitations, err := s.store.listWorkspaces(ctx, user, email)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workspaces: %v", err)
	}
	resp := &platformv1.ListWorkspacesResponse{
		Workspaces:  make([]*platformv1.Workspace, 0, len(workspaces)),
		Invitations: toProtoWorkspaceInvitations(invitations),
	}
	for _, rec := range workspaces {
		resp.Workspaces = append(resp.Workspaces, toProtoWorkspace(rec))
	}
	return resp, nil
}

func (s *platformService) GetWorkspace(ctx context.Context, req *platformv1.GetWorkspaceRequest) (*platformv1.Workspace, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.workspaceByID(ctx, user, workspaceID)
	if err != nil {
		return nil, readAccessError("workspace", err)
	}
	return toProtoWorkspace(rec), nil
}

func (s *platformService) CreateWorkspace(ctx context.Context, req *platformv1.CreateWorkspaceRequest) (*platformv1.Workspace, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.createWorkspace(ctx, user, req.GetName())
	if err != nil {
		return nil, workspaceError("create workspace", err)
	}
	return toProtoWorkspace(rec), nil
}

func (s *platformService) RenameWorkspace(ctx context.Context, req *platformv1.RenameWorkspaceRequest) (*platformv1.Workspace, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.renameWorkspace(ctx, user, workspaceID, req.GetName())
	if err != nil {
		return nil, workspaceError("rename workspace", err)
	}
	return toProtoWorkspace(rec), nil
}

func (s *platformService) DeleteWorkspace(ctx context.Context, req *platformv1.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteWorkspace(ctx, user, workspaceID, req.GetConfirmationName()); err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, workspaceError("delete workspace", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *platformService) ListWorkspaceMembers(ctx context.Context, req *platformv1.ListWorkspaceMembersRequest) (*platformv1.ListWorkspaceMembersResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	members, invitations, err := s.store.listWorkspaceMembers(ctx, user, workspaceID)
	if err != nil {
		return nil, readAccessError("list workspace members", err)
	}
	resp := &platformv1.ListWorkspaceMembersResponse{
		Members:     make([]*platformv1.WorkspaceMember, 0, len(members)),
		Invitations: toProtoWorkspaceInvitations(invitations),
	}
	for _, rec := range members {
		resp.Members = append(resp.Members, toProtoWorkspaceMember(rec))
	}
	return resp, nil
}

func (s *platformService) InviteWorkspaceMember(ctx context.Context, req *platformv1.InviteWorkspaceMemberRequest) (*platformv1.WorkspaceInvitation, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	role, err := roleFromProto(req.GetRole())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.inviteWorkspaceMember(ctx, user, workspaceID, req.GetEmail(), role)
	if err != nil {
		return nil, workspaceError("invite workspace member", err)
	}
	return toProtoWorkspaceInvitation(rec), nil
}

func (s *platformService) RevokeWorkspaceInvitation(ctx context.Context, req *platformv1.RevokeWorkspaceInvitationRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	invitationID, err := requiredID("invitation_id", req.GetInvitationId())
	if err != nil {
		return nil, err
	}
	if err := s.store.revokeWorkspaceInvitation(ctx, user, invitationID); err != nil {
		return nil, workspaceError("revoke workspace invitation", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *platformService) AcceptWorkspaceInvitation(ctx context.Context, req *platformv1.AcceptWorkspaceInvitationRequest) (*platformv1.Workspace, error) {
	user, email, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	invitationID, err := requiredID("invitation_id", req.GetInvitationId())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.acceptWorkspaceInvitation(ctx, user, email, invitationID)
	if err != nil {
		return nil, workspaceError("accept workspace invitation", err)
	}
	return toProtoWorkspace(rec), nil
}

func (s *platformService) DeclineWorkspaceInvitation(ctx context.Context, req *platformv1.DeclineWorkspaceInvitationRequest) (*emptypb.Empty, error) {
	user, email, err := delegatedUserWithEmail(ctx)
	if err != nil {
		return nil, err
	}
	invitationID, err := requiredID("invitation_id", req.GetInvitationId())
	if err != nil {
		return nil, err
	}
	if err := s.store.declineWorkspaceInvitation(ctx, user, email, invitationID); err != nil {
		return nil, workspaceError("decline workspace invitation", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *platformService) UpdateWorkspaceMemberRole(ctx context.Context, req *platformv1.UpdateWorkspaceMemberRoleRequest) (*platformv1.WorkspaceMember, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	memberUserID, err := requiredID("user_id", req.GetUserId())
	if err != nil {
		return nil, err
	}
	role, err := roleFromProto(req.GetRole())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.updateWorkspaceMemberRole(ctx, user, workspaceID, memberUserID, role)
	if err != nil {
		return nil, workspaceError("update workspace member role", err)
	}
	return toProtoWorkspaceMember(rec), nil
}

func (s *platformService) RemoveWorkspaceMember(ctx context.Context, req *platformv1.RemoveWorkspaceMemberRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	memberUserID, err := requiredID("user_id", req.GetUserId())
	if err != nil {
		return nil, err
	}
	if err := s.store.removeWorkspaceMember(ctx, user, workspaceID, memberUserID); err != nil {
		return nil, workspaceError("remove workspace member", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *platformService) MoveProject(ctx context.Context, req *platformv1.MoveProjectRequest) (*platformv1.Project, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	projectID, err := requiredID("project_id", req.GetProjectId())
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredID("workspace_id", req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.moveProject(ctx, user, projectID, workspaceID)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, workspaceError("move project", err)
	}
	return toProtoProject(rec), nil
}
