package apiclient

import (
	"context"
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	return &out, c.do(ctx, http.MethodGet, "/v1/me", nil, nil, &out)
}

func (c *Client) Whoami(ctx context.Context) (*TokenInfo, error) {
	var out TokenInfo
	return &out, c.do(ctx, http.MethodGet, "/v1/token", nil, nil, &out)
}

func (c *Client) ListWorkspaces(ctx context.Context) (*platformv1.ListWorkspacesResponse, error) {
	out := &platformv1.ListWorkspacesResponse{}
	return out, c.do(ctx, http.MethodGet, "/v1/workspaces", nil, nil, out)
}

func (c *Client) CreateWorkspace(ctx context.Context, req *platformv1.CreateWorkspaceRequest) (*platformv1.Workspace, error) {
	out := &platformv1.Workspace{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces", nil, req, out)
}

func (c *Client) GetWorkspace(ctx context.Context, workspaceID string) (*platformv1.Workspace, error) {
	out := &platformv1.Workspace{}
	return out, c.do(ctx, http.MethodGet, "/v1/workspaces/"+escape(workspaceID), nil, nil, out)
}

func (c *Client) RenameWorkspace(ctx context.Context, workspaceID, name string) (*platformv1.Workspace, error) {
	out := &platformv1.Workspace{}
	return out, c.do(ctx, http.MethodPatch, "/v1/workspaces/"+escape(workspaceID), nil, map[string]string{"name": name}, out)
}

func (c *Client) DeleteWorkspace(ctx context.Context, workspaceID, confirmationName string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspaces/"+escape(workspaceID), nil, map[string]string{"confirmation_name": confirmationName}, nil)
}

func (c *Client) ListWorkspaceMembers(ctx context.Context, workspaceID string) (*platformv1.ListWorkspaceMembersResponse, error) {
	out := &platformv1.ListWorkspaceMembersResponse{}
	return out, c.do(ctx, http.MethodGet, "/v1/workspaces/"+escape(workspaceID)+"/members", nil, nil, out)
}

func (c *Client) InviteWorkspaceMember(ctx context.Context, req *platformv1.InviteWorkspaceMemberRequest) (*platformv1.WorkspaceInvitation, error) {
	out := &platformv1.WorkspaceInvitation{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+escape(req.GetWorkspaceId())+"/invitations", nil, req, out)
}

func (c *Client) RevokeWorkspaceInvitation(ctx context.Context, invitationID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspace-invitations/"+escape(invitationID), nil, nil, nil)
}

func (c *Client) AcceptWorkspaceInvitation(ctx context.Context, invitationID string) (*platformv1.Workspace, error) {
	out := &platformv1.Workspace{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspace-invitations/"+escape(invitationID)+"/accept", nil, nil, out)
}

func (c *Client) DeclineWorkspaceInvitation(ctx context.Context, invitationID string) error {
	return c.do(ctx, http.MethodPost, "/v1/workspace-invitations/"+escape(invitationID)+"/decline", nil, nil, nil)
}

func (c *Client) UpdateWorkspaceMemberRole(ctx context.Context, workspaceID, userID string, role platformv1.WorkspaceRole) (*platformv1.WorkspaceMember, error) {
	out := &platformv1.WorkspaceMember{}
	req := &platformv1.UpdateWorkspaceMemberRoleRequest{WorkspaceId: workspaceID, UserId: userID, Role: role}
	return out, c.do(ctx, http.MethodPatch, "/v1/workspaces/"+escape(workspaceID)+"/members/"+escape(userID), nil, req, out)
}

func (c *Client) RemoveWorkspaceMember(ctx context.Context, workspaceID, userID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspaces/"+escape(workspaceID)+"/members/"+escape(userID), nil, nil, nil)
}

func (c *Client) ListProjects(ctx context.Context, req *platformv1.ListProjectsRequest) (*platformv1.ListProjectsResponse, error) {
	out := &platformv1.ListProjectsResponse{}
	q := query(map[string]string{
		"workspace_id":      req.GetWorkspaceId(),
		"include_deleted":   boolQuery(req.GetIncludeDeleted()),
		"include_summaries": boolQuery(req.GetIncludeSummaries()),
	})
	return out, c.do(ctx, http.MethodGet, "/v1/projects", q, nil, out)
}

func (c *Client) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	out := &platformv1.Project{}
	return out, c.do(ctx, http.MethodPost, "/v1/projects", nil, req, out)
}

func (c *Client) GetProject(ctx context.Context, projectID string) (*platformv1.Project, error) {
	out := &platformv1.Project{}
	return out, c.do(ctx, http.MethodGet, "/v1/projects/"+escape(projectID), nil, nil, out)
}

func (c *Client) DeleteProject(ctx context.Context, projectID, confirmationName string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+escape(projectID), nil, map[string]string{"confirmation_name": confirmationName}, nil)
}

func (c *Client) RestoreProject(ctx context.Context, projectID string) (*platformv1.Project, error) {
	out := &platformv1.Project{}
	return out, c.do(ctx, http.MethodPost, "/v1/projects/"+escape(projectID)+"/restore", nil, nil, out)
}

func (c *Client) UpdateProjectLogRetention(ctx context.Context, projectID string, days int32) (*platformv1.Project, error) {
	out := &platformv1.Project{}
	return out, c.do(ctx, http.MethodPatch, "/v1/projects/"+escape(projectID)+"/log-retention", nil, map[string]int32{"log_retention_days": days}, out)
}

func (c *Client) PreviewProjectDeletion(ctx context.Context, projectID string) (*platformv1.DeletionPreview, error) {
	out := &platformv1.DeletionPreview{}
	return out, c.do(ctx, http.MethodGet, "/v1/projects/"+escape(projectID)+"/deletion-preview", nil, nil, out)
}

func (c *Client) MoveProject(ctx context.Context, projectID, workspaceID string) (*platformv1.Project, error) {
	out := &platformv1.Project{}
	return out, c.do(ctx, http.MethodPost, "/v1/projects/"+escape(projectID)+"/move", nil, map[string]string{"workspace_id": workspaceID}, out)
}

func (c *Client) ListEnvironments(ctx context.Context, projectID string, includeDeleted bool) (*platformv1.ListEnvironmentsResponse, error) {
	out := &platformv1.ListEnvironmentsResponse{}
	q := query(map[string]string{"include_deleted": boolQuery(includeDeleted)})
	return out, c.do(ctx, http.MethodGet, "/v1/projects/"+escape(projectID)+"/environments", q, nil, out)
}

func (c *Client) CreateEnvironment(ctx context.Context, projectID, name string) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	return out, c.do(ctx, http.MethodPost, "/v1/projects/"+escape(projectID)+"/environments", nil, map[string]string{"name": name}, out)
}

func (c *Client) GetEnvironment(ctx context.Context, environmentID string) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	return out, c.do(ctx, http.MethodGet, "/v1/environments/"+escape(environmentID), nil, nil, out)
}

func (c *Client) RenameEnvironment(ctx context.Context, environmentID, name string) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	return out, c.do(ctx, http.MethodPatch, "/v1/environments/"+escape(environmentID), nil, map[string]string{"name": name}, out)
}

func (c *Client) UpdateEnvironmentAutoDeploy(ctx context.Context, environmentID string, autoDeploy bool) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	return out, c.do(ctx, http.MethodPatch, "/v1/environments/"+escape(environmentID)+"/auto-deploy", nil, map[string]bool{"auto_deploy": autoDeploy}, out)
}

func (c *Client) DuplicateEnvironment(ctx context.Context, environmentID, name string, copyVariables bool) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	body := map[string]any{"name": name, "copy_variables": copyVariables}
	return out, c.do(ctx, http.MethodPost, "/v1/environments/"+escape(environmentID)+"/duplicate", nil, body, out)
}

func (c *Client) DeleteEnvironment(ctx context.Context, environmentID, confirmationName string) error {
	return c.do(ctx, http.MethodDelete, "/v1/environments/"+escape(environmentID), nil, map[string]string{"confirmation_name": confirmationName}, nil)
}

func (c *Client) RestoreEnvironment(ctx context.Context, environmentID string) (*platformv1.Environment, error) {
	out := &platformv1.Environment{}
	return out, c.do(ctx, http.MethodPost, "/v1/environments/"+escape(environmentID)+"/restore", nil, nil, out)
}

func (c *Client) PreviewEnvironmentDeletion(ctx context.Context, environmentID string) (*platformv1.DeletionPreview, error) {
	out := &platformv1.DeletionPreview{}
	return out, c.do(ctx, http.MethodGet, "/v1/environments/"+escape(environmentID)+"/deletion-preview", nil, nil, out)
}

func (c *Client) ReleaseEnvironment(ctx context.Context, environmentID string) (*platformv1.ReleaseEnvironmentResponse, error) {
	out := &platformv1.ReleaseEnvironmentResponse{}
	return out, c.do(ctx, http.MethodPost, "/v1/environments/"+escape(environmentID)+"/releases", nil, nil, out)
}

func (c *Client) ListServices(ctx context.Context, environmentID string, includeDeleted bool) (*platformv1.ListServicesResponse, error) {
	out := &platformv1.ListServicesResponse{}
	q := query(map[string]string{"include_deleted": boolQuery(includeDeleted)})
	return out, c.do(ctx, http.MethodGet, "/v1/environments/"+escape(environmentID)+"/services", q, nil, out)
}

func (c *Client) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	return out, c.do(ctx, http.MethodPost, "/v1/environments/"+escape(req.GetEnvironmentId())+"/services", nil, req, out)
}

func (c *Client) GetService(ctx context.Context, serviceID string) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID), nil, nil, out)
}

// GetServiceWithSecrets reveals env values. The token needs full access and
// its user secret-read access; every other read returns names only.
func (c *Client) GetServiceWithSecrets(ctx context.Context, serviceID string) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	q := query(map[string]string{"include_secrets": boolQuery(true)})
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID), q, nil, out)
}

func (c *Client) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	return out, c.do(ctx, http.MethodPut, "/v1/services/"+escape(req.GetServiceId()), nil, req, out)
}

func (c *Client) DeleteService(ctx context.Context, serviceID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/services/"+escape(serviceID), nil, nil, nil)
}

func (c *Client) RestoreService(ctx context.Context, serviceID string) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	return out, c.do(ctx, http.MethodPost, "/v1/services/"+escape(serviceID)+"/restore", nil, nil, out)
}

func (c *Client) GetServiceStatus(ctx context.Context, serviceID string) (*platformv1.ServiceStatus, error) {
	out := &platformv1.ServiceStatus{}
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID)+"/status", nil, nil, out)
}

func (c *Client) ScaleService(ctx context.Context, serviceID string, replicas int32) (*platformv1.ServiceStatus, error) {
	out := &platformv1.ServiceStatus{}
	return out, c.do(ctx, http.MethodPost, "/v1/services/"+escape(serviceID)+"/scale", nil, map[string]int32{"desired_replica_count": replicas}, out)
}

func (c *Client) DiscardServiceChanges(ctx context.Context, req *platformv1.DiscardServiceChangesRequest) (*platformv1.Service, error) {
	out := &platformv1.Service{}
	return out, c.do(ctx, http.MethodPost, "/v1/services/"+escape(req.GetServiceId())+"/discard-changes", nil, req, out)
}

func (c *Client) ListDeployments(ctx context.Context, serviceID string, limit int32) (*platformv1.ListServiceDeploymentsResponse, error) {
	out := &platformv1.ListServiceDeploymentsResponse{}
	q := query(map[string]string{"limit": intQuery(limit)})
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID)+"/deployments", q, nil, out)
}

func (c *Client) DeploymentAction(ctx context.Context, req *platformv1.ApplyDeploymentActionRequest) (*platformv1.ServiceStatus, error) {
	out := &platformv1.ServiceStatus{}
	path := "/v1/services/" + escape(req.GetServiceId()) + "/deployments/" + escape(req.GetDeploymentId()) + "/actions"
	return out, c.do(ctx, http.MethodPost, path, nil, req, out)
}

func (c *Client) ListLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	out := &platformv1.ListServiceLogsResponse{}
	values := map[string]string{
		"limit": intQuery(req.GetLimit()), "search": req.GetSearch(),
		"build_id": req.GetBuildId(), "allocation_id": req.GetAllocationId(),
		"page_token": req.GetPageToken(), "gap_page_token": req.GetGapPageToken(),
	}
	if req.GetLogType() != platformv1.ServiceLogType_SERVICE_LOG_TYPE_UNSPECIFIED {
		values["log_type"] = req.GetLogType().String()
	}
	if ts := req.GetStartTime(); ts != nil {
		values["start_time"] = ts.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	if ts := req.GetEndTime(); ts != nil {
		values["end_time"] = ts.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(req.GetServiceId())+"/logs", query(values), nil, out)
}

func (c *Client) ListArtifacts(ctx context.Context, serviceID string, limit int32) (*platformv1.ListServiceArtifactsResponse, error) {
	out := &platformv1.ListServiceArtifactsResponse{}
	q := query(map[string]string{"limit": intQuery(limit)})
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID)+"/artifacts", q, nil, out)
}

func (c *Client) ListBuildAttempts(ctx context.Context, serviceID, buildID string) (*platformv1.ListBuildAttemptsResponse, error) {
	out := &platformv1.ListBuildAttemptsResponse{}
	path := "/v1/services/" + escape(serviceID) + "/builds/" + escape(buildID) + "/attempts"
	return out, c.do(ctx, http.MethodGet, path, nil, nil, out)
}

func (c *Client) ListDomains(ctx context.Context, serviceID string, includeDeleted bool) (*platformv1.ListDomainBindingsResponse, error) {
	out := &platformv1.ListDomainBindingsResponse{}
	q := query(map[string]string{"include_deleted": boolQuery(includeDeleted)})
	return out, c.do(ctx, http.MethodGet, "/v1/services/"+escape(serviceID)+"/domains", q, nil, out)
}

func (c *Client) CreateDomain(ctx context.Context, input *platformv1.DomainBindingInput) (*platformv1.DomainBinding, error) {
	out := &platformv1.DomainBinding{}
	return out, c.do(ctx, http.MethodPost, "/v1/services/"+escape(input.GetServiceId())+"/domains", nil, input, out)
}

func (c *Client) GenerateDomain(ctx context.Context, serviceID string, targetPort int32) (*platformv1.DomainBinding, error) {
	out := &platformv1.DomainBinding{}
	req := &platformv1.GenerateDomainBindingRequest{ServiceId: serviceID, TargetPort: targetPort}
	return out, c.do(ctx, http.MethodPost, "/v1/services/"+escape(serviceID)+"/generated-domain", nil, req, out)
}

func (c *Client) GetDomain(ctx context.Context, hostname string) (*platformv1.DomainBinding, error) {
	out := &platformv1.DomainBinding{}
	return out, c.do(ctx, http.MethodGet, "/v1/domains/"+escape(hostname), nil, nil, out)
}

func (c *Client) UpdateDomain(ctx context.Context, hostname string, target *platformv1.DomainBindingTarget) (*platformv1.DomainBinding, error) {
	out := &platformv1.DomainBinding{}
	return out, c.do(ctx, http.MethodPatch, "/v1/domains/"+escape(hostname), nil, target, out)
}

func (c *Client) DeleteDomain(ctx context.Context, hostname string) error {
	return c.do(ctx, http.MethodDelete, "/v1/domains/"+escape(hostname), nil, nil, nil)
}

func (c *Client) RestoreDomain(ctx context.Context, hostname string) (*platformv1.DomainBinding, error) {
	out := &platformv1.DomainBinding{}
	return out, c.do(ctx, http.MethodPost, "/v1/domains/"+escape(hostname)+"/restore", nil, nil, out)
}

func (c *Client) ListVolumes(ctx context.Context, environmentID string, includeDeleted bool) (*platformv1.ListVolumesResponse, error) {
	out := &platformv1.ListVolumesResponse{}
	q := query(map[string]string{"include_deleted": boolQuery(includeDeleted)})
	return out, c.do(ctx, http.MethodGet, "/v1/environments/"+escape(environmentID)+"/volumes", q, nil, out)
}

func (c *Client) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	out := &platformv1.Volume{}
	return out, c.do(ctx, http.MethodPost, "/v1/environments/"+escape(req.GetEnvironmentId())+"/volumes", nil, req, out)
}

func (c *Client) UpdateVolume(ctx context.Context, volumeID string, sizeBytes int64) (*platformv1.Volume, error) {
	out := &platformv1.Volume{}
	return out, c.do(ctx, http.MethodPatch, "/v1/volumes/"+escape(volumeID), nil, map[string]int64{"size_bytes": sizeBytes}, out)
}

func (c *Client) DeleteVolume(ctx context.Context, volumeID, confirmationName string) error {
	return c.do(ctx, http.MethodDelete, "/v1/volumes/"+escape(volumeID), nil, map[string]string{"confirmation_name": confirmationName}, nil)
}

func (c *Client) PreviewVolumeDeletion(ctx context.Context, volumeID string) (*platformv1.DeletionPreview, error) {
	out := &platformv1.DeletionPreview{}
	return out, c.do(ctx, http.MethodGet, "/v1/volumes/"+escape(volumeID)+"/deletion-preview", nil, nil, out)
}

func (c *Client) InspectSource(ctx context.Context, req *platformv1.InspectSourceRequest) (*platformv1.InspectSourceResponse, error) {
	out := &platformv1.InspectSourceResponse{}
	return out, c.do(ctx, http.MethodPost, "/v1/source-inspections", nil, req, out)
}

func (c *Client) LinkGitHubRepository(ctx context.Context, req *platformv1.LinkGitHubRepositoryRequest) (*platformv1.InspectSourceResponse, error) {
	out := &platformv1.InspectSourceResponse{}
	return out, c.do(ctx, http.MethodPost, "/v1/projects/"+escape(req.GetProjectId())+"/github-repository", nil, req, out)
}

func (c *Client) GetProjectUsage(ctx context.Context, req *platformv1.GetProjectUsageRequest) (*platformv1.ProjectUsage, error) {
	out := &platformv1.ProjectUsage{}
	values := map[string]string{
		"environment_id": req.GetEnvironmentId(), "service_id": req.GetServiceId(),
		"allocation_id": req.GetAllocationId(), "volume_id": req.GetVolumeId(),
		"resource_buckets": boolQuery(req.GetResourceBuckets()),
	}
	if req.GetResolution() != platformv1.UsageResolution_USAGE_RESOLUTION_UNSPECIFIED {
		values["resolution"] = req.GetResolution().String()
	}
	if ts := req.GetStartTime(); ts != nil {
		values["start_time"] = ts.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	if ts := req.GetEndTime(); ts != nil {
		values["end_time"] = ts.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	return out, c.do(ctx, http.MethodGet, "/v1/projects/"+escape(req.GetProjectId())+"/usage", query(values), nil, out)
}

func (c *Client) ListAgents(ctx context.Context) (*platformv1.ListAgentsResponse, error) {
	out := &platformv1.ListAgentsResponse{}
	return out, c.do(ctx, http.MethodGet, "/v1/fleet/agents", nil, nil, out)
}

func (c *Client) GetBillingAccount(ctx context.Context, workspaceID string) (*platformv1.BillingAccountSummary, error) {
	out := &platformv1.BillingAccountSummary{}
	return out, c.do(ctx, http.MethodGet, "/v1/workspaces/"+escape(workspaceID)+"/billing", nil, nil, out)
}

func (c *Client) ListBillingStatements(ctx context.Context, workspaceID string, limit int32) (*platformv1.ListBillingStatementsResponse, error) {
	out := &platformv1.ListBillingStatementsResponse{}
	q := query(map[string]string{"limit": intQuery(limit)})
	return out, c.do(ctx, http.MethodGet, "/v1/workspaces/"+escape(workspaceID)+"/billing/statements", q, nil, out)
}

func (c *Client) GetBillingStatement(ctx context.Context, workspaceID, statementID string, includeCSV bool) (*platformv1.BillingStatement, error) {
	out := &platformv1.BillingStatement{}
	q := query(map[string]string{"include_csv": boolQuery(includeCSV)})
	path := "/v1/workspaces/" + escape(workspaceID) + "/billing/statements/" + escape(statementID)
	return out, c.do(ctx, http.MethodGet, path, q, nil, out)
}

func (c *Client) UpdateBillingBudget(ctx context.Context, workspaceID string, budgetStopCents int64) (*platformv1.BillingAccountSummary, error) {
	out := &platformv1.BillingAccountSummary{}
	return out, c.do(ctx, http.MethodPatch, "/v1/workspaces/"+escape(workspaceID)+"/billing/budget", nil, map[string]int64{"budget_stop_cents": budgetStopCents}, out)
}

func (c *Client) ChangeBillingPlan(ctx context.Context, workspaceID, planID string) (*platformv1.BillingAccountSummary, error) {
	out := &platformv1.BillingAccountSummary{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+escape(workspaceID)+"/billing/plan", nil, map[string]string{"plan_id": planID}, out)
}

func (c *Client) StartBillingPaymentSetup(ctx context.Context, workspaceID string) (*platformv1.StartBillingPaymentSetupResponse, error) {
	out := &platformv1.StartBillingPaymentSetupResponse{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+escape(workspaceID)+"/billing/payment-setup", nil, nil, out)
}

func (c *Client) StartBillingActivation(ctx context.Context, workspaceID, planID string) (*platformv1.BillingAccountSummary, error) {
	out := &platformv1.BillingAccountSummary{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+escape(workspaceID)+"/billing/activation", nil, map[string]string{"plan_id": planID}, out)
}

func (c *Client) CancelBillingAccount(ctx context.Context, workspaceID string) (*platformv1.CancelBillingAccountResponse, error) {
	out := &platformv1.CancelBillingAccountResponse{}
	return out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+escape(workspaceID)+"/billing/cancel", nil, nil, out)
}

func (c *Client) GetVolume(ctx context.Context, volumeID string) (*platformv1.Volume, error) {
	out := &platformv1.Volume{}
	return out, c.do(ctx, http.MethodGet, "/v1/volumes/"+escape(volumeID), nil, nil, out)
}
