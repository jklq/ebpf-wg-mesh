package mcpserver

import (
	"context"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type empty struct{}

type workspaceIDIn struct {
	WorkspaceID string `json:"workspace_id" jsonschema:"workspace ID (defaults to linked workspace)"`
}

func (s *state) addWorkspaceTools(server *mcp.Server) {
	add(server, readTool("whoami", "Who the configured API token acts as, its scope and access. Call first to orient."), func(ctx context.Context, _ empty) (any, error) {
		info, err := s.client.Whoami(ctx)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"token": info}
		if info.Scope == "account" {
			if me, err := s.client.Me(ctx); err == nil {
				out["user_id"] = me.UserID
				out["email"] = me.Email
			}
		}
		if s.defaults != (Defaults{}) {
			out["linked_defaults"] = s.defaults
		}
		return out, nil
	})

	add(server, readTool("list_workspaces", "List workspaces the token's user belongs to, with pending invitations (account/workspace tokens)."), func(ctx context.Context, _ empty) (any, error) {
		return s.client.ListWorkspaces(ctx)
	})

	add(server, readTool("get_workspace", "Get one workspace."), func(ctx context.Context, in workspaceIDIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.GetWorkspace(ctx, id)
	})

	type createWorkspaceIn struct {
		Name string `json:"name" jsonschema:"workspace name"`
	}
	add(server, writeTool("create_workspace", "Create a workspace. Requires an account-scoped token."), func(ctx context.Context, in createWorkspaceIn) (any, error) {
		return s.client.CreateWorkspace(ctx, &platformv1.CreateWorkspaceRequest{Name: in.Name})
	})

	type renameWorkspaceIn struct {
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name" jsonschema:"new workspace name"`
	}
	add(server, writeTool("rename_workspace", "Rename a workspace."), func(ctx context.Context, in renameWorkspaceIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.RenameWorkspace(ctx, id, in.Name)
	})

	type deleteWorkspaceIn struct {
		WorkspaceID string `json:"workspace_id"`
		Confirm     bool   `json:"confirm,omitempty" jsonschema:"must be true to execute; without it a preview is returned"`
	}
	add(server, destructiveTool("delete_workspace", "Delete a shared workspace that has no live projects. Destructive: preview first, then confirm:true."), func(ctx context.Context, in deleteWorkspaceIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		workspace, err := s.client.GetWorkspace(ctx, id)
		if err != nil {
			return nil, err
		}
		if !in.Confirm {
			return preview(fmt.Sprintf("delete workspace %q (%s); its recently deleted projects become unrestorable", workspace.GetName(), id), map[string]any{"workspace": protoJSON(workspace)}), nil
		}
		if err := s.client.DeleteWorkspace(ctx, id, workspace.GetName()); err != nil {
			return nil, err
		}
		return deleted("workspace", id, workspace.GetName()), nil
	})

	add(server, readTool("list_workspace_members", "List members and pending invitations of a workspace."), func(ctx context.Context, in workspaceIDIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.ListWorkspaceMembers(ctx, id)
	})

	type inviteIn struct {
		WorkspaceID string `json:"workspace_id"`
		Email       string `json:"email" jsonschema:"invitee email; they accept by signing in with this address"`
		Role        string `json:"role" jsonschema:"admin, member, or viewer"`
	}
	add(server, writeTool("invite_workspace_member", "Invite someone to a workspace by email."), func(ctx context.Context, in inviteIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		role, err := parseRole(in.Role)
		if err != nil {
			return nil, err
		}
		return s.client.InviteWorkspaceMember(ctx, &platformv1.InviteWorkspaceMemberRequest{WorkspaceId: id, Email: in.Email, Role: role})
	})

	type invitationIn struct {
		InvitationID string `json:"invitation_id"`
	}
	add(server, writeTool("revoke_workspace_invitation", "Revoke a pending workspace invitation."), func(ctx context.Context, in invitationIn) (any, error) {
		if err := s.client.RevokeWorkspaceInvitation(ctx, in.InvitationID); err != nil {
			return nil, err
		}
		return map[string]any{"revoked_invitation_id": in.InvitationID}, nil
	})
	add(server, writeTool("accept_workspace_invitation", "Accept an invitation addressed to the token's user."), func(ctx context.Context, in invitationIn) (any, error) {
		return s.client.AcceptWorkspaceInvitation(ctx, in.InvitationID)
	})
	add(server, writeTool("decline_workspace_invitation", "Decline an invitation addressed to the token's user."), func(ctx context.Context, in invitationIn) (any, error) {
		if err := s.client.DeclineWorkspaceInvitation(ctx, in.InvitationID); err != nil {
			return nil, err
		}
		return map[string]any{"declined_invitation_id": in.InvitationID}, nil
	})

	type memberRoleIn struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
		Role        string `json:"role" jsonschema:"admin, member, or viewer"`
	}
	add(server, writeTool("update_workspace_member_role", "Change a workspace member's role."), func(ctx context.Context, in memberRoleIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		role, err := parseRole(in.Role)
		if err != nil {
			return nil, err
		}
		return s.client.UpdateWorkspaceMemberRole(ctx, id, in.UserID, role)
	})

	type removeMemberIn struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id" jsonschema:"member to remove; your own user ID leaves the workspace"`
		Confirm     bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("remove_workspace_member", "Remove a member from a workspace. Destructive: preview first, then confirm:true."), func(ctx context.Context, in removeMemberIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		if !in.Confirm {
			return preview(fmt.Sprintf("remove user %s from workspace %s", in.UserID, id), nil), nil
		}
		if err := s.client.RemoveWorkspaceMember(ctx, id, in.UserID); err != nil {
			return nil, err
		}
		return map[string]any{"removed_user_id": in.UserID, "workspace_id": id}, nil
	})
}

func parseRole(raw string) (platformv1.WorkspaceRole, error) {
	switch raw {
	case "admin":
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_ADMIN, nil
	case "member":
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_MEMBER, nil
	case "viewer":
		return platformv1.WorkspaceRole_WORKSPACE_ROLE_VIEWER, nil
	default:
		if value, ok := platformv1.WorkspaceRole_value[raw]; ok {
			return platformv1.WorkspaceRole(value), nil
		}
		return 0, fmt.Errorf("role must be admin, member, or viewer")
	}
}
