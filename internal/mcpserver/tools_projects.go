package mcpserver

import (
	"context"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type projectIDIn struct {
	ProjectID string `json:"project_id" jsonschema:"project ID (defaults to linked project)"`
}

type environmentIDIn struct {
	EnvironmentID string `json:"environment_id" jsonschema:"environment ID (defaults to linked environment)"`
}

func (s *state) addProjectTools(server *mcp.Server) {
	type listProjectsIn struct {
		WorkspaceID      string `json:"workspace_id,omitempty" jsonschema:"only this workspace; empty lists every workspace of the user"`
		IncludeDeleted   bool   `json:"include_deleted,omitempty"`
		IncludeSummaries bool   `json:"include_summaries,omitempty" jsonschema:"include environment/service summaries for an overview"`
	}
	add(server, readTool("list_projects", "List projects. Start here to discover project IDs."), func(ctx context.Context, in listProjectsIn) (any, error) {
		return s.client.ListProjects(ctx, &platformv1.ListProjectsRequest{
			WorkspaceId: in.WorkspaceID, IncludeDeleted: in.IncludeDeleted, IncludeSummaries: in.IncludeSummaries,
		})
	})

	type createProjectIn struct {
		Name        string `json:"name"`
		WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"empty creates in the personal workspace (account tokens) or the scoped workspace"`
	}
	add(server, writeTool("create_project", "Create a project. It starts with a production environment."), func(ctx context.Context, in createProjectIn) (any, error) {
		return s.client.CreateProject(ctx, &platformv1.CreateProjectRequest{Name: in.Name, WorkspaceId: in.WorkspaceID})
	})

	add(server, readTool("get_project", "Get one project."), func(ctx context.Context, in projectIDIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.GetProject(ctx, id)
	})

	type deleteProjectIn struct {
		ProjectID string `json:"project_id"`
		Confirm   bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("delete_project", "Delete a project with all environments, services, domains, and volumes (restorable during the grace period). Destructive: preview first, then confirm:true."), func(ctx context.Context, in deleteProjectIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		project, err := s.client.GetProject(ctx, id)
		if err != nil {
			return nil, err
		}
		if !in.Confirm {
			detail, err := s.client.PreviewProjectDeletion(ctx, id)
			if err != nil {
				return nil, err
			}
			return preview(fmt.Sprintf("delete project %q (%s)", project.GetName(), id), map[string]any{"would_tombstone": protoJSON(detail)}), nil
		}
		if err := s.client.DeleteProject(ctx, id, project.GetName()); err != nil {
			return nil, err
		}
		return deleted("project", id, project.GetName()), nil
	})

	add(server, writeTool("restore_project", "Restore a deleted project within its grace period."), func(ctx context.Context, in projectIDIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.RestoreProject(ctx, id)
	})

	type moveProjectIn struct {
		ProjectID   string `json:"project_id"`
		WorkspaceID string `json:"workspace_id" jsonschema:"target workspace ID"`
	}
	add(server, writeTool("move_project", "Move a project to another workspace."), func(ctx context.Context, in moveProjectIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.MoveProject(ctx, id, in.WorkspaceID)
	})

	type logRetentionIn struct {
		ProjectID       string `json:"project_id"`
		LogRetentionDay int32  `json:"log_retention_days" jsonschema:"1-90 days; 0 resets to the platform default"`
	}
	add(server, idempotentWriteTool("update_project_log_retention", "Set how long the project's logs are retained."), func(ctx context.Context, in logRetentionIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.UpdateProjectLogRetention(ctx, id, in.LogRetentionDay)
	})

	type listEnvironmentsIn struct {
		ProjectID      string `json:"project_id"`
		IncludeDeleted bool   `json:"include_deleted,omitempty"`
	}
	add(server, readTool("list_environments", "List environments of a project. Environments are the deployment and private-network scopes."), func(ctx context.Context, in listEnvironmentsIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.ListEnvironments(ctx, id, in.IncludeDeleted)
	})

	type createEnvironmentIn struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}
	add(server, writeTool("create_environment", "Create an empty environment in a project."), func(ctx context.Context, in createEnvironmentIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.CreateEnvironment(ctx, id, in.Name)
	})

	add(server, readTool("get_environment", "Get one environment."), func(ctx context.Context, in environmentIDIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.GetEnvironment(ctx, id)
	})

	add(server, readTool("environment_status", "Environment overview: services with live status, volumes, and staged-change counts. Good first look at an environment."), func(ctx context.Context, in environmentIDIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		env, err := s.client.GetEnvironment(ctx, id)
		if err != nil {
			return nil, err
		}
		listed, err := s.client.ListServices(ctx, id, false)
		if err != nil {
			return nil, err
		}
		services := make([]any, 0, len(listed.GetServices()))
		for _, svc := range listed.GetServices() {
			status, statusErr := s.client.GetServiceStatus(ctx, svc.GetId())
			entry := map[string]any{"service": protoJSON(svc)}
			if statusErr == nil {
				entry["allocations"] = protoJSONList(status.GetAllocations())
			} else {
				entry["status_error"] = statusErr.Error()
			}
			services = append(services, entry)
		}
		return map[string]any{
			"environment": protoJSON(env),
			"services":    services,
			"volumes":     protoJSONList(listed.GetVolumes()),
		}, nil
	})

	type renameEnvironmentIn struct {
		EnvironmentID string `json:"environment_id"`
		Name          string `json:"name"`
	}
	add(server, writeTool("rename_environment", "Rename an environment."), func(ctx context.Context, in renameEnvironmentIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.RenameEnvironment(ctx, id, in.Name)
	})

	type duplicateEnvironmentIn struct {
		EnvironmentID string `json:"environment_id" jsonschema:"source environment"`
		Name          string `json:"name" jsonschema:"name for the copy"`
		CopyVariables bool   `json:"copy_variables,omitempty" jsonschema:"also copy service environment variables"`
	}
	add(server, writeTool("duplicate_environment", "Duplicate an environment's service configuration into a new environment."), func(ctx context.Context, in duplicateEnvironmentIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.DuplicateEnvironment(ctx, id, in.Name, in.CopyVariables)
	})

	type autoDeployIn struct {
		EnvironmentID string `json:"environment_id"`
		AutoDeploy    bool   `json:"auto_deploy" jsonschema:"deploy automatically when a tracked source ref moves"`
	}
	add(server, idempotentWriteTool("set_environment_auto_deploy", "Toggle auto-deploy for an environment."), func(ctx context.Context, in autoDeployIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.UpdateEnvironmentAutoDeploy(ctx, id, in.AutoDeploy)
	})

	type deleteEnvironmentIn struct {
		EnvironmentID string `json:"environment_id"`
		Confirm       bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("delete_environment", "Delete an environment with its services, domains, and volumes (restorable during grace). Destructive: preview first, then confirm:true."), func(ctx context.Context, in deleteEnvironmentIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		env, err := s.client.GetEnvironment(ctx, id)
		if err != nil {
			return nil, err
		}
		if !in.Confirm {
			detail, err := s.client.PreviewEnvironmentDeletion(ctx, id)
			if err != nil {
				return nil, err
			}
			return preview(fmt.Sprintf("delete environment %q (%s)", env.GetName(), id), map[string]any{"would_tombstone": protoJSON(detail)}), nil
		}
		confirmation := ""
		if env.GetIsProduction() {
			confirmation = env.GetName()
		}
		if err := s.client.DeleteEnvironment(ctx, id, confirmation); err != nil {
			return nil, err
		}
		return deleted("environment", id, env.GetName()), nil
	})

	add(server, writeTool("restore_environment", "Restore a deleted environment within its grace period."), func(ctx context.Context, in environmentIDIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.RestoreEnvironment(ctx, id)
	})

	type releaseIn struct {
		EnvironmentID string `json:"environment_id"`
		Confirm       bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("release_environment", "Deploy: apply every staged service change in the environment (Railway's accept-deploy). Without confirm:true this returns the staged changes as a preview."), func(ctx context.Context, in releaseIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		if !in.Confirm {
			listed, err := s.client.ListServices(ctx, id, false)
			if err != nil {
				return nil, err
			}
			staged := make([]any, 0)
			for _, svc := range listed.GetServices() {
				if svc.GetPendingChanges() {
					staged = append(staged, map[string]any{
						"service_id": svc.GetId(), "name": svc.GetName(),
						"unapplied_changes": protoJSONList(svc.GetUnappliedChanges()),
					})
				}
			}
			if len(staged) == 0 {
				return map[string]any{"preview": true, "executed": false, "summary": "no staged changes; releasing would be a no-op", "staged": staged}, nil
			}
			return preview(fmt.Sprintf("release %d service(s) with staged changes in environment %s", len(staged), id), map[string]any{"staged": staged}), nil
		}
		resp, err := s.client.ReleaseEnvironment(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"released": true, "environment_id": id, "services": protoJSONList(resp.GetServices())}, nil
	})
	// deploy is the Railway-familiar alias for releasing.
	add(server, destructiveTool("deploy", "Alias of release_environment: deploy every staged change. Without confirm:true returns a preview."), func(ctx context.Context, in struct {
		EnvironmentID string `json:"environment_id"`
		ServiceID     string `json:"service_id,omitempty" jsonschema:"accepted for familiarity; releases apply to the whole environment"`
		Confirm       bool   `json:"confirm,omitempty"`
	}) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if id == "" && in.ServiceID != "" {
			svc, err := s.client.GetService(ctx, in.ServiceID)
			if err != nil {
				return nil, err
			}
			id = svc.GetEnvironmentId()
		}
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		if !in.Confirm {
			return map[string]any{"preview": true, "executed": false, "requires_confirm": true,
				"summary": "release environment " + id, "next_step": "Call release_environment to inspect staged changes, or repeat with confirm: true to deploy."}, nil
		}
		resp, err := s.client.ReleaseEnvironment(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"released": true, "environment_id": id, "services": protoJSONList(resp.GetServices())}, nil
	})

	s.addUsageTools(server)
	s.addSourceTools(server)
}

func (s *state) addUsageTools(server *mcp.Server) {
	type usageIn struct {
		ProjectID       string `json:"project_id"`
		StartTime       string `json:"start_time,omitempty" jsonschema:"RFC3339 start; default 24h ago (minute), 7d (hour), 30d (day)"`
		EndTime         string `json:"end_time,omitempty" jsonschema:"RFC3339 end; default now"`
		Resolution      string `json:"resolution,omitempty" jsonschema:"minute, hour, or day (default hour)"`
		EnvironmentID   string `json:"environment_id,omitempty"`
		ServiceID       string `json:"service_id,omitempty"`
		VolumeID        string `json:"volume_id,omitempty"`
		ResourceBuckets bool   `json:"resource_buckets,omitempty" jsonschema:"include per-resource bucket series"`
	}
	add(server, readTool("get_project_usage", "Resource usage for a project: CPU, memory, volume, and egress with coverage and gaps. Quantities are exact integers as strings in resource units (CPU microseconds, byte-milliseconds, bytes)."), func(ctx context.Context, in usageIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		req, err := usageRequestFrom(id, in.StartTime, in.EndTime, in.Resolution)
		if err != nil {
			return nil, err
		}
		req.EnvironmentId, req.ServiceId, req.VolumeId = in.EnvironmentID, in.ServiceID, in.VolumeID
		req.ResourceBuckets = in.ResourceBuckets
		return s.client.GetProjectUsage(ctx, req)
	})

	type metricsIn struct {
		ServiceID  string `json:"service_id"`
		Resolution string `json:"resolution,omitempty" jsonschema:"minute, hour, or day (default hour)"`
		Hours      int    `json:"hours,omitempty" jsonschema:"lookback window in hours (default 24)"`
	}
	add(server, readTool("service_metrics", "CPU/memory/volume/egress usage for one service over a lookback window (derived from project usage)."), func(ctx context.Context, in metricsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		svc, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		env, err := s.client.GetEnvironment(ctx, svc.GetEnvironmentId())
		if err != nil {
			return nil, err
		}
		hours := in.Hours
		if hours <= 0 {
			hours = 24
		}
		end := time.Now().UTC()
		start := end.Add(-time.Duration(hours) * time.Hour)
		req, err := usageRequestFrom(env.GetProjectId(), start.Format(time.RFC3339), end.Format(time.RFC3339), in.Resolution)
		if err != nil {
			return nil, err
		}
		req.ServiceId = id
		req.ResourceBuckets = true
		return s.client.GetProjectUsage(ctx, req)
	})
}

func usageRequestFrom(projectID, startRaw, endRaw, resolutionRaw string) (*platformv1.GetProjectUsageRequest, error) {
	resolution := platformv1.UsageResolution_USAGE_RESOLUTION_HOUR
	switch resolutionRaw {
	case "", "hour":
	case "minute":
		resolution = platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE
	case "day":
		resolution = platformv1.UsageResolution_USAGE_RESOLUTION_DAY
	default:
		if value, ok := platformv1.UsageResolution_value[resolutionRaw]; ok {
			resolution = platformv1.UsageResolution(value)
		} else {
			return nil, fmt.Errorf("resolution must be minute, hour, or day")
		}
	}
	now := time.Now().UTC()
	end := now
	if endRaw != "" {
		parsed, err := time.Parse(time.RFC3339, endRaw)
		if err != nil {
			return nil, fmt.Errorf("end_time must be RFC3339: %w", err)
		}
		end = parsed
	}
	start := end.Add(-7 * 24 * time.Hour)
	switch resolution {
	case platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE:
		start = end.Add(-24 * time.Hour)
	case platformv1.UsageResolution_USAGE_RESOLUTION_DAY:
		start = end.Add(-30 * 24 * time.Hour)
	}
	if startRaw != "" {
		parsed, err := time.Parse(time.RFC3339, startRaw)
		if err != nil {
			return nil, fmt.Errorf("start_time must be RFC3339: %w", err)
		}
		start = parsed
	}
	return &platformv1.GetProjectUsageRequest{
		ProjectId: projectID, Resolution: resolution,
		StartTime: timestamppb.New(start), EndTime: timestamppb.New(end),
	}, nil
}

func (s *state) addSourceTools(server *mcp.Server) {
	type inspectIn struct {
		RepositorySelector    string `json:"repository_selector" jsonschema:"e.g. github.com/owner/repo or owner/repo as configured by the provider"`
		Provider              string `json:"provider,omitempty" jsonschema:"source provider (default github)"`
		ProjectID             string `json:"project_id,omitempty"`
		GitHubUserAccessToken string `json:"github_user_access_token,omitempty" jsonschema:"transient GitHub OAuth token proving repository access; never stored"`
	}
	tool := readTool("inspect_source", "Inspect a source repository: access state, default branch, recommended build recipe, ports, language, and start command.")
	tool.Annotations.OpenWorldHint = boolPtr(true)
	add(server, tool, func(ctx context.Context, in inspectIn) (any, error) {
		provider := in.Provider
		if provider == "" {
			provider = "github"
		}
		return s.client.InspectSource(ctx, &platformv1.InspectSourceRequest{
			Provider: provider, RepositorySelector: in.RepositorySelector,
			ProjectId: s.projectID(in.ProjectID), GithubUserAccessToken: in.GitHubUserAccessToken,
		})
	})

	type linkIn struct {
		ProjectID             string `json:"project_id"`
		RepositorySelector    string `json:"repository_selector"`
		GitHubUserAccessToken string `json:"github_user_access_token,omitempty"`
	}
	linkTool := writeTool("link_github_repository", "Link a GitHub repository to the project and return the source inspection used to create services from it.")
	linkTool.Annotations.OpenWorldHint = boolPtr(true)
	add(server, linkTool, func(ctx context.Context, in linkIn) (any, error) {
		id := s.projectID(in.ProjectID)
		if err := required("project_id", id); err != nil {
			return nil, err
		}
		return s.client.LinkGitHubRepository(ctx, &platformv1.LinkGitHubRepositoryRequest{
			ProjectId: id, RepositorySelector: in.RepositorySelector, GithubUserAccessToken: in.GitHubUserAccessToken,
		})
	})
}

// protoJSONList renders a slice of proto messages as plain JSON values.
func protoJSONList[M proto.Message](items []M) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, protoJSON(item))
	}
	return out
}
