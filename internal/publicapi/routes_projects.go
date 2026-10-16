package publicapi

import (
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *Server) projectRoutes() []routeEntry {
	projWS := scopeRule{minScope: tierWorkspace, kind: "project", param: "projectId"}
	projWSWrite := scopeRule{minScope: tierWorkspace, kind: "project", param: "projectId", write: true}
	envWS := scopeRule{minScope: tierWorkspace, kind: "environment", param: "environmentId"}
	envWSWrite := scopeRule{minScope: tierWorkspace, kind: "environment", param: "environmentId", write: true}
	envAny := scopeRule{kind: "environment", param: "environmentId"}
	envAnyWrite := scopeRule{kind: "environment", param: "environmentId", write: true}
	entries := []routeEntry{
		{method: "GET", pattern: "/v1/projects", summary: "List projects (workspace_id, include_deleted, include_summaries)", rule: scopeRule{minScope: tierWorkspace}, resp: "platform.v1.ListProjectsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			workspaceID := r.URL.Query().Get("workspace_id")
			if err := checkWorkspaceSelector(token, workspaceID); err != nil {
				s.writeError(w, r, err)
				return
			}
			if token.Scope == ScopeWorkspace && workspaceID == "" {
				workspaceID = token.WorkspaceID
			}
			includeDeleted, err := queryBool(r, "include_deleted")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			includeSummaries, err := queryBool(r, "include_summaries")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListProjects(r.Context(), &platformv1.ListProjectsRequest{
				WorkspaceId: workspaceID, IncludeDeleted: includeDeleted, IncludeSummaries: includeSummaries,
			})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/projects", summary: "Create a project (empty workspace_id uses the personal workspace)", rule: scopeRule{minScope: tierWorkspace, write: true}, req: "platform.v1.CreateProjectRequest", resp: "platform.v1.Project", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			req := &platformv1.CreateProjectRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			if err := checkWorkspaceSelector(token, req.GetWorkspaceId()); err != nil {
				s.writeError(w, r, err)
				return
			}
			if token.Scope == ScopeWorkspace && req.GetWorkspaceId() == "" {
				req.WorkspaceId = token.WorkspaceID
			}
			resp, err := s.platform.CreateProject(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "GET", pattern: "/v1/projects/{projectId}", summary: "Get a project", rule: scopeRule{kind: "project", param: "projectId"}, resp: "platform.v1.Project", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetProject(r.Context(), &platformv1.GetProjectRequest{ProjectId: r.PathValue("projectId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/projects/{projectId}", summary: "Delete a project (confirmation_name must match; restorable during grace)", rule: projWSWrite, handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ConfirmationName string `json:"confirmation_name"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.DeleteProject(r.Context(), &platformv1.DeleteProjectRequest{ProjectId: r.PathValue("projectId"), ConfirmationName: body.ConfirmationName})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/projects/{projectId}/restore", summary: "Restore a deleted project", rule: projWSWrite, resp: "platform.v1.Project", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RestoreProject(r.Context(), &platformv1.RestoreProjectRequest{ProjectId: r.PathValue("projectId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/projects/{projectId}/log-retention", summary: "Set log retention days (1-90, 0 resets to default)", rule: projWSWrite, resp: "platform.v1.Project", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				LogRetentionDays int32 `json:"log_retention_days"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.UpdateProjectLogRetention(r.Context(), &platformv1.UpdateProjectLogRetentionRequest{ProjectId: r.PathValue("projectId"), LogRetentionDays: body.LogRetentionDays})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/projects/{projectId}/deletion-preview", summary: "Preview what deleting the project would tombstone", rule: projWS, resp: "platform.v1.DeletionPreview", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.PreviewProjectDeletion(r.Context(), &platformv1.PreviewProjectDeletionRequest{ProjectId: r.PathValue("projectId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/projects/{projectId}/move", summary: "Move a project to another workspace", rule: scopeRule{minScope: tierAccount, kind: "project", param: "projectId", write: true}, resp: "platform.v1.Project", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				WorkspaceID string `json:"workspace_id"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.MoveProject(r.Context(), &platformv1.MoveProjectRequest{ProjectId: r.PathValue("projectId"), WorkspaceId: body.WorkspaceID})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/projects/{projectId}/usage", summary: "Project resource usage (start_time, end_time, resolution, filters)", rule: scopeRule{kind: "project", param: "projectId"}, resp: "platform.v1.ProjectUsage", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			req, err := usageRequest(r)
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ProjectId = r.PathValue("projectId")
			if token.Scope == ScopeProject {
				if req.GetEnvironmentId() != "" && req.GetEnvironmentId() != token.EnvironmentID {
					s.writeError(w, r, forbiddenScope("this token is scoped to a different project environment"))
					return
				}
				req.EnvironmentId = token.EnvironmentID
				if req.GetServiceId() != "" {
					if err := s.checkResourceScope(r.Context(), token, "service", req.GetServiceId()); err != nil {
						s.writeError(w, r, err)
						return
					}
				}
				if req.GetVolumeId() != "" {
					if err := s.checkResourceScope(r.Context(), token, "volume", req.GetVolumeId()); err != nil {
						s.writeError(w, r, err)
						return
					}
				}
			}
			resp, err := s.platform.GetProjectUsage(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/projects/{projectId}/environments", summary: "List environments (include_deleted)", rule: projWS, resp: "platform.v1.ListEnvironmentsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			includeDeleted, err := queryBool(r, "include_deleted")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListEnvironments(r.Context(), &platformv1.ListEnvironmentsRequest{ProjectId: r.PathValue("projectId"), IncludeDeleted: includeDeleted})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/projects/{projectId}/environments", summary: "Create an environment", rule: projWSWrite, req: "platform.v1.CreateEnvironmentRequest", resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.CreateEnvironmentRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ProjectId = r.PathValue("projectId")
			resp, err := s.platform.CreateEnvironment(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "POST", pattern: "/v1/projects/{projectId}/github-repository", summary: "Link and inspect a GitHub repository for the project", rule: projWSWrite, req: "platform.v1.LinkGitHubRepositoryRequest", resp: "platform.v1.InspectSourceResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.LinkGitHubRepositoryRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ProjectId = r.PathValue("projectId")
			resp, err := s.platform.LinkGitHubRepository(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
	}
	return append(entries, environmentEntries(s, envWS, envWSWrite, envAny, envAnyWrite)...)
}

func environmentEntries(s *Server, envWS, envWSWrite, envAny, envAnyWrite scopeRule) []routeEntry {
	return []routeEntry{
		{method: "GET", pattern: "/v1/environments/{environmentId}", summary: "Get an environment", rule: envAny, resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetEnvironment(r.Context(), &platformv1.GetEnvironmentRequest{EnvironmentId: r.PathValue("environmentId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/environments/{environmentId}", summary: "Rename an environment", rule: envWSWrite, resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name string `json:"name"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.RenameEnvironment(r.Context(), &platformv1.RenameEnvironmentRequest{EnvironmentId: r.PathValue("environmentId"), Name: body.Name})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/environments/{environmentId}/auto-deploy", summary: "Toggle auto-deploy on tracked-ref changes", rule: envAnyWrite, resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				AutoDeploy bool `json:"auto_deploy"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.UpdateEnvironmentAutoDeploy(r.Context(), &platformv1.UpdateEnvironmentAutoDeployRequest{EnvironmentId: r.PathValue("environmentId"), AutoDeploy: body.AutoDeploy})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/environments/{environmentId}/duplicate", summary: "Duplicate an environment (copy_variables optional)", rule: envWSWrite, resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name          string `json:"name"`
				CopyVariables bool   `json:"copy_variables"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.DuplicateEnvironment(r.Context(), &platformv1.DuplicateEnvironmentRequest{SourceEnvironmentId: r.PathValue("environmentId"), Name: body.Name, CopyVariables: body.CopyVariables})
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/environments/{environmentId}", summary: "Delete an environment (production requires confirmation_name)", rule: envWSWrite, handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ConfirmationName string `json:"confirmation_name"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.DeleteEnvironment(r.Context(), &platformv1.DeleteEnvironmentRequest{EnvironmentId: r.PathValue("environmentId"), ConfirmationName: body.ConfirmationName})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/environments/{environmentId}/restore", summary: "Restore a deleted environment", rule: envWSWrite, resp: "platform.v1.Environment", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RestoreEnvironment(r.Context(), &platformv1.RestoreEnvironmentRequest{EnvironmentId: r.PathValue("environmentId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/environments/{environmentId}/deletion-preview", summary: "Preview what deleting the environment would tombstone", rule: envWS, resp: "platform.v1.DeletionPreview", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.PreviewEnvironmentDeletion(r.Context(), &platformv1.PreviewEnvironmentDeletionRequest{EnvironmentId: r.PathValue("environmentId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/environments/{environmentId}/releases", summary: "Release the environment: deploy every staged service change", rule: envAnyWrite, resp: "platform.v1.ReleaseEnvironmentResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.ReleaseEnvironment(r.Context(), &platformv1.ReleaseEnvironmentRequest{EnvironmentId: r.PathValue("environmentId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/fleet/agents", summary: "List fleet agents (operators only)", rule: scopeRule{minScope: tierAccount}, resp: "platform.v1.ListAgentsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.ListAgents(r.Context(), &emptypb.Empty{})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/source-inspections", summary: "Inspect a source repository without linking it", rule: scopeRule{minScope: tierWorkspace}, req: "platform.v1.InspectSourceRequest", resp: "platform.v1.InspectSourceResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			req := &platformv1.InspectSourceRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			if err := s.checkResourceScope(r.Context(), token, "project", req.GetProjectId()); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.InspectSource(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
	}
}

// usageRequest builds a GetProjectUsageRequest from query parameters.
func usageRequest(r *http.Request) (*platformv1.GetProjectUsageRequest, error) {
	start, err := queryTime(r, "start_time")
	if err != nil {
		return nil, err
	}
	end, err := queryTime(r, "end_time")
	if err != nil {
		return nil, err
	}
	resourceBuckets, err := queryBool(r, "resource_buckets")
	if err != nil {
		return nil, err
	}
	req := &platformv1.GetProjectUsageRequest{
		StartTime: start, EndTime: end, ResourceBuckets: resourceBuckets,
		EnvironmentId: r.URL.Query().Get("environment_id"),
		ServiceId:     r.URL.Query().Get("service_id"),
		AllocationId:  r.URL.Query().Get("allocation_id"),
		VolumeId:      r.URL.Query().Get("volume_id"),
	}
	if raw := r.URL.Query().Get("resolution"); raw != "" {
		value, ok := platformv1.UsageResolution_value[raw]
		if !ok {
			// Friendly aliases: minute, hour, day.
			value, ok = platformv1.UsageResolution_value["USAGE_RESOLUTION_"+asciiUpper(raw)]
		}
		if !ok {
			return nil, badRequest("query parameter resolution must be minute, hour, or day")
		}
		req.Resolution = platformv1.UsageResolution(value)
	}
	return req, nil
}

func asciiUpper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - ('a' - 'A')
		}
	}
	return string(out)
}
