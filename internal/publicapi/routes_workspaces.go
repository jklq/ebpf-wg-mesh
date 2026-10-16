package publicapi

import (
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *Server) workspaceRoutes() []routeEntry {
	ws := scopeRule{minScope: tierWorkspace, kind: "workspace", param: "workspaceId"}
	wsWriteAccount := scopeRule{minScope: tierAccount, kind: "workspace", param: "workspaceId", write: true}
	return []routeEntry{
		{method: "GET", pattern: "/v1/workspaces", summary: "List the caller's workspaces and pending invitations", rule: scopeRule{minScope: tierWorkspace}, resp: "platform.v1.ListWorkspacesResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.ListWorkspaces(r.Context(), &platformv1.ListWorkspacesRequest{})
			if err == nil {
				if token, _ := tokenFromContext(r.Context()); token.Scope == ScopeWorkspace {
					kept := resp.Workspaces[:0]
					for _, item := range resp.Workspaces {
						if item.GetId() == token.WorkspaceID {
							kept = append(kept, item)
						}
					}
					resp.Workspaces, resp.Invitations = kept, nil
				}
			}
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces", summary: "Create a workspace", rule: scopeRule{minScope: tierAccount, write: true}, req: "platform.v1.CreateWorkspaceRequest", resp: "platform.v1.Workspace", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.CreateWorkspaceRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.CreateWorkspace(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "GET", pattern: "/v1/workspaces/{workspaceId}", summary: "Get a workspace", rule: ws, resp: "platform.v1.Workspace", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetWorkspace(r.Context(), &platformv1.GetWorkspaceRequest{WorkspaceId: r.PathValue("workspaceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/workspaces/{workspaceId}", summary: "Rename a workspace", rule: wsWriteAccount, req: "platform.v1.RenameWorkspaceRequest", resp: "platform.v1.Workspace", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.RenameWorkspaceRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.WorkspaceId = r.PathValue("workspaceId")
			resp, err := s.platform.RenameWorkspace(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/workspaces/{workspaceId}", summary: "Delete a workspace with no live projects (confirmation_name must match)", rule: wsWriteAccount, handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ConfirmationName string `json:"confirmation_name"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.DeleteWorkspace(r.Context(), &platformv1.DeleteWorkspaceRequest{WorkspaceId: r.PathValue("workspaceId"), ConfirmationName: body.ConfirmationName})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/workspaces/{workspaceId}/members", summary: "List workspace members and invitations", rule: ws, resp: "platform.v1.ListWorkspaceMembersResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.ListWorkspaceMembers(r.Context(), &platformv1.ListWorkspaceMembersRequest{WorkspaceId: r.PathValue("workspaceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces/{workspaceId}/invitations", summary: "Invite a member by email", rule: wsWriteAccount, req: "platform.v1.InviteWorkspaceMemberRequest", resp: "platform.v1.WorkspaceInvitation", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.InviteWorkspaceMemberRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.WorkspaceId = r.PathValue("workspaceId")
			resp, err := s.platform.InviteWorkspaceMember(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/workspaces/{workspaceId}/members/{userId}", summary: "Change a member's role", rule: wsWriteAccount, req: "platform.v1.UpdateWorkspaceMemberRoleRequest", resp: "platform.v1.WorkspaceMember", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.UpdateWorkspaceMemberRoleRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.WorkspaceId, req.UserId = r.PathValue("workspaceId"), r.PathValue("userId")
			resp, err := s.platform.UpdateWorkspaceMemberRole(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/workspaces/{workspaceId}/members/{userId}", summary: "Remove a member (removing yourself leaves the workspace)", rule: wsWriteAccount, handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RemoveWorkspaceMember(r.Context(), &platformv1.RemoveWorkspaceMemberRequest{WorkspaceId: r.PathValue("workspaceId"), UserId: r.PathValue("userId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/workspace-invitations/{invitationId}", summary: "Revoke an invitation", rule: scopeRule{minScope: tierAccount, write: true}, handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RevokeWorkspaceInvitation(r.Context(), &platformv1.RevokeWorkspaceInvitationRequest{InvitationId: r.PathValue("invitationId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspace-invitations/{invitationId}/accept", summary: "Accept an invitation addressed to the caller", rule: scopeRule{minScope: tierAccount, write: true}, resp: "platform.v1.Workspace", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.AcceptWorkspaceInvitation(r.Context(), &platformv1.AcceptWorkspaceInvitationRequest{InvitationId: r.PathValue("invitationId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspace-invitations/{invitationId}/decline", summary: "Decline an invitation addressed to the caller", rule: scopeRule{minScope: tierAccount, write: true}, handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.DeclineWorkspaceInvitation(r.Context(), &platformv1.DeclineWorkspaceInvitationRequest{InvitationId: r.PathValue("invitationId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
	}
}
