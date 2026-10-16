package controlplane

import (
	"context"
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"connectrpc.com/connect"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type connectPlatformService struct {
	service *platformService
}

type connectOpsService struct {
	service *opsService
}

func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}
	return connect.NewError(connect.Code(uint32(status.Code(err))), err)
}

func (a connectPlatformService) ListWorkspaces(ctx context.Context, req *connect.Request[platformv1.ListWorkspacesRequest]) (*connect.Response[platformv1.ListWorkspacesResponse], error) {
	result, err := a.service.ListWorkspaces(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetWorkspace(ctx context.Context, req *connect.Request[platformv1.GetWorkspaceRequest]) (*connect.Response[platformv1.Workspace], error) {
	result, err := a.service.GetWorkspace(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateWorkspace(ctx context.Context, req *connect.Request[platformv1.CreateWorkspaceRequest]) (*connect.Response[platformv1.Workspace], error) {
	result, err := a.service.CreateWorkspace(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RenameWorkspace(ctx context.Context, req *connect.Request[platformv1.RenameWorkspaceRequest]) (*connect.Response[platformv1.Workspace], error) {
	result, err := a.service.RenameWorkspace(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteWorkspace(ctx context.Context, req *connect.Request[platformv1.DeleteWorkspaceRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteWorkspace(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListWorkspaceMembers(ctx context.Context, req *connect.Request[platformv1.ListWorkspaceMembersRequest]) (*connect.Response[platformv1.ListWorkspaceMembersResponse], error) {
	result, err := a.service.ListWorkspaceMembers(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) InviteWorkspaceMember(ctx context.Context, req *connect.Request[platformv1.InviteWorkspaceMemberRequest]) (*connect.Response[platformv1.WorkspaceInvitation], error) {
	result, err := a.service.InviteWorkspaceMember(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RevokeWorkspaceInvitation(ctx context.Context, req *connect.Request[platformv1.RevokeWorkspaceInvitationRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.RevokeWorkspaceInvitation(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) AcceptWorkspaceInvitation(ctx context.Context, req *connect.Request[platformv1.AcceptWorkspaceInvitationRequest]) (*connect.Response[platformv1.Workspace], error) {
	result, err := a.service.AcceptWorkspaceInvitation(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeclineWorkspaceInvitation(ctx context.Context, req *connect.Request[platformv1.DeclineWorkspaceInvitationRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeclineWorkspaceInvitation(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateWorkspaceMemberRole(ctx context.Context, req *connect.Request[platformv1.UpdateWorkspaceMemberRoleRequest]) (*connect.Response[platformv1.WorkspaceMember], error) {
	result, err := a.service.UpdateWorkspaceMemberRole(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RemoveWorkspaceMember(ctx context.Context, req *connect.Request[platformv1.RemoveWorkspaceMemberRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.RemoveWorkspaceMember(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) MoveProject(ctx context.Context, req *connect.Request[platformv1.MoveProjectRequest]) (*connect.Response[platformv1.Project], error) {
	result, err := a.service.MoveProject(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateProject(ctx context.Context, req *connect.Request[platformv1.CreateProjectRequest]) (*connect.Response[platformv1.Project], error) {
	result, err := a.service.CreateProject(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListProjects(ctx context.Context, req *connect.Request[platformv1.ListProjectsRequest]) (*connect.Response[platformv1.ListProjectsResponse], error) {
	result, err := a.service.ListProjects(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetProject(ctx context.Context, req *connect.Request[platformv1.GetProjectRequest]) (*connect.Response[platformv1.Project], error) {
	result, err := a.service.GetProject(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListEnvironments(ctx context.Context, req *connect.Request[platformv1.ListEnvironmentsRequest]) (*connect.Response[platformv1.ListEnvironmentsResponse], error) {
	result, err := a.service.ListEnvironments(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetEnvironment(ctx context.Context, req *connect.Request[platformv1.GetEnvironmentRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.GetEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateEnvironment(ctx context.Context, req *connect.Request[platformv1.CreateEnvironmentRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.CreateEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DuplicateEnvironment(ctx context.Context, req *connect.Request[platformv1.DuplicateEnvironmentRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.DuplicateEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RenameEnvironment(ctx context.Context, req *connect.Request[platformv1.RenameEnvironmentRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.RenameEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateEnvironmentAutoDeploy(ctx context.Context, req *connect.Request[platformv1.UpdateEnvironmentAutoDeployRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.UpdateEnvironmentAutoDeploy(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteEnvironment(ctx context.Context, req *connect.Request[platformv1.DeleteEnvironmentRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RestoreEnvironment(ctx context.Context, req *connect.Request[platformv1.RestoreEnvironmentRequest]) (*connect.Response[platformv1.Environment], error) {
	result, err := a.service.RestoreEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteProject(ctx context.Context, req *connect.Request[platformv1.DeleteProjectRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteProject(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RestoreProject(ctx context.Context, req *connect.Request[platformv1.RestoreProjectRequest]) (*connect.Response[platformv1.Project], error) {
	result, err := a.service.RestoreProject(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateProjectLogRetention(ctx context.Context, req *connect.Request[platformv1.UpdateProjectLogRetentionRequest]) (*connect.Response[platformv1.Project], error) {
	result, err := a.service.UpdateProjectLogRetention(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) PreviewProjectDeletion(ctx context.Context, req *connect.Request[platformv1.PreviewProjectDeletionRequest]) (*connect.Response[platformv1.DeletionPreview], error) {
	result, err := a.service.PreviewProjectDeletion(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) PreviewEnvironmentDeletion(ctx context.Context, req *connect.Request[platformv1.PreviewEnvironmentDeletionRequest]) (*connect.Response[platformv1.DeletionPreview], error) {
	result, err := a.service.PreviewEnvironmentDeletion(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) PreviewVolumeDeletion(ctx context.Context, req *connect.Request[platformv1.PreviewVolumeDeletionRequest]) (*connect.Response[platformv1.DeletionPreview], error) {
	result, err := a.service.PreviewVolumeDeletion(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RestoreService(ctx context.Context, req *connect.Request[platformv1.RestoreServiceRequest]) (*connect.Response[platformv1.Service], error) {
	result, err := a.service.RestoreService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RestoreDomainBinding(ctx context.Context, req *connect.Request[platformv1.RestoreDomainBindingRequest]) (*connect.Response[platformv1.DomainBinding], error) {
	result, err := a.service.RestoreDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ReleaseEnvironment(ctx context.Context, req *connect.Request[platformv1.ReleaseEnvironmentRequest]) (*connect.Response[platformv1.ReleaseEnvironmentResponse], error) {
	result, err := a.service.ReleaseEnvironment(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) LinkGitHubRepository(ctx context.Context, req *connect.Request[platformv1.LinkGitHubRepositoryRequest]) (*connect.Response[platformv1.InspectSourceResponse], error) {
	result, err := a.service.LinkGitHubRepository(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) InspectSource(ctx context.Context, req *connect.Request[platformv1.InspectSourceRequest]) (*connect.Response[platformv1.InspectSourceResponse], error) {
	result, err := a.service.InspectSource(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateService(ctx context.Context, req *connect.Request[platformv1.CreateServiceRequest]) (*connect.Response[platformv1.Service], error) {
	result, err := a.service.CreateService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateService(ctx context.Context, req *connect.Request[platformv1.UpdateServiceRequest]) (*connect.Response[platformv1.Service], error) {
	result, err := a.service.UpdateService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ScaleService(ctx context.Context, req *connect.Request[platformv1.ScaleServiceRequest]) (*connect.Response[platformv1.ServiceStatus], error) {
	result, err := a.service.ScaleService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ApplyDeploymentAction(ctx context.Context, req *connect.Request[platformv1.ApplyDeploymentActionRequest]) (*connect.Response[platformv1.ServiceStatus], error) {
	result, err := a.service.ApplyDeploymentAction(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DiscardServiceChanges(ctx context.Context, req *connect.Request[platformv1.DiscardServiceChangesRequest]) (*connect.Response[platformv1.Service], error) {
	result, err := a.service.DiscardServiceChanges(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteService(ctx context.Context, req *connect.Request[platformv1.DeleteServiceRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetService(ctx context.Context, req *connect.Request[platformv1.GetServiceRequest]) (*connect.Response[platformv1.Service], error) {
	result, err := a.service.GetService(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListServices(ctx context.Context, req *connect.Request[platformv1.ListServicesRequest]) (*connect.Response[platformv1.ListServicesResponse], error) {
	result, err := a.service.ListServices(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateVolume(ctx context.Context, req *connect.Request[platformv1.CreateVolumeRequest]) (*connect.Response[platformv1.Volume], error) {
	result, err := a.service.CreateVolume(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateVolume(ctx context.Context, req *connect.Request[platformv1.UpdateVolumeRequest]) (*connect.Response[platformv1.Volume], error) {
	result, err := a.service.UpdateVolume(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteVolume(ctx context.Context, req *connect.Request[platformv1.DeleteVolumeRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteVolume(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListVolumes(ctx context.Context, req *connect.Request[platformv1.ListVolumesRequest]) (*connect.Response[platformv1.ListVolumesResponse], error) {
	result, err := a.service.ListVolumes(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateDomainBinding(ctx context.Context, req *connect.Request[platformv1.CreateDomainBindingRequest]) (*connect.Response[platformv1.DomainBinding], error) {
	result, err := a.service.CreateDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GenerateDomainBinding(ctx context.Context, req *connect.Request[platformv1.GenerateDomainBindingRequest]) (*connect.Response[platformv1.DomainBinding], error) {
	result, err := a.service.GenerateDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetDomainBinding(ctx context.Context, req *connect.Request[platformv1.GetDomainBindingRequest]) (*connect.Response[platformv1.DomainBinding], error) {
	result, err := a.service.GetDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListDomainBindings(ctx context.Context, req *connect.Request[platformv1.ListDomainBindingsRequest]) (*connect.Response[platformv1.ListDomainBindingsResponse], error) {
	result, err := a.service.ListDomainBindings(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateDomainBinding(ctx context.Context, req *connect.Request[platformv1.UpdateDomainBindingRequest]) (*connect.Response[platformv1.DomainBinding], error) {
	result, err := a.service.UpdateDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) DeleteDomainBinding(ctx context.Context, req *connect.Request[platformv1.DeleteDomainBindingRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.DeleteDomainBinding(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetServiceStatus(ctx context.Context, req *connect.Request[platformv1.GetServiceStatusRequest]) (*connect.Response[platformv1.ServiceStatus], error) {
	result, err := a.service.GetServiceStatus(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListServiceLogs(ctx context.Context, req *connect.Request[platformv1.ListServiceLogsRequest]) (*connect.Response[platformv1.ListServiceLogsResponse], error) {
	result, err := a.service.ListServiceLogs(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListServiceDeployments(ctx context.Context, req *connect.Request[platformv1.ListServiceDeploymentsRequest]) (*connect.Response[platformv1.ListServiceDeploymentsResponse], error) {
	result, err := a.service.ListServiceDeployments(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListServiceArtifacts(ctx context.Context, req *connect.Request[platformv1.ListServiceArtifactsRequest]) (*connect.Response[platformv1.ListServiceArtifactsResponse], error) {
	result, err := a.service.ListServiceArtifacts(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListAgents(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[platformv1.ListAgentsResponse], error) {
	result, err := a.service.ListAgents(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetProjectUsage(ctx context.Context, req *connect.Request[platformv1.GetProjectUsageRequest]) (*connect.Response[platformv1.ProjectUsage], error) {
	result, err := a.service.GetProjectUsage(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(result), nil
}

func (a connectPlatformService) GetBillingAccount(ctx context.Context, req *connect.Request[platformv1.GetBillingAccountRequest]) (*connect.Response[platformv1.BillingAccountSummary], error) {
	result, err := a.service.GetBillingAccount(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListBillingStatements(ctx context.Context, req *connect.Request[platformv1.ListBillingStatementsRequest]) (*connect.Response[platformv1.ListBillingStatementsResponse], error) {
	result, err := a.service.ListBillingStatements(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) GetBillingStatement(ctx context.Context, req *connect.Request[platformv1.GetBillingStatementRequest]) (*connect.Response[platformv1.BillingStatement], error) {
	result, err := a.service.GetBillingStatement(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) UpdateBillingBudget(ctx context.Context, req *connect.Request[platformv1.UpdateBillingBudgetRequest]) (*connect.Response[platformv1.BillingAccountSummary], error) {
	result, err := a.service.UpdateBillingBudget(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ChangeBillingPlan(ctx context.Context, req *connect.Request[platformv1.ChangeBillingPlanRequest]) (*connect.Response[platformv1.BillingAccountSummary], error) {
	result, err := a.service.ChangeBillingPlan(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) StartBillingPaymentSetup(ctx context.Context, req *connect.Request[platformv1.StartBillingPaymentSetupRequest]) (*connect.Response[platformv1.StartBillingPaymentSetupResponse], error) {
	result, err := a.service.StartBillingPaymentSetup(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) StartBillingActivation(ctx context.Context, req *connect.Request[platformv1.StartBillingActivationRequest]) (*connect.Response[platformv1.BillingAccountSummary], error) {
	result, err := a.service.StartBillingActivation(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CancelBillingAccount(ctx context.Context, req *connect.Request[platformv1.CancelBillingAccountRequest]) (*connect.Response[platformv1.CancelBillingAccountResponse], error) {
	result, err := a.service.CancelBillingAccount(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) CreateAPIToken(ctx context.Context, req *connect.Request[platformv1.CreateAPITokenRequest]) (*connect.Response[platformv1.CreateAPITokenResponse], error) {
	result, err := a.service.CreateAPIToken(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListAPITokens(ctx context.Context, req *connect.Request[platformv1.ListAPITokensRequest]) (*connect.Response[platformv1.ListAPITokensResponse], error) {
	result, err := a.service.ListAPITokens(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) RevokeAPIToken(ctx context.Context, req *connect.Request[platformv1.RevokeAPITokenRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.RevokeAPIToken(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectPlatformService) ListBuildAttempts(ctx context.Context, req *connect.Request[platformv1.ListBuildAttemptsRequest]) (*connect.Response[platformv1.ListBuildAttemptsResponse], error) {
	result, err := a.service.ListBuildAttempts(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) IngestGitHubWebhook(ctx context.Context, req *connect.Request[platformv1.IngestGitHubWebhookRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.IngestGitHubWebhook(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) IngestStripeWebhook(ctx context.Context, req *connect.Request[platformv1.IngestStripeWebhookRequest]) (*connect.Response[emptypb.Empty], error) {
	result, err := a.service.IngestStripeWebhook(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) SetBillingCollectionPaused(ctx context.Context, req *connect.Request[platformv1.SetBillingCollectionPausedRequest]) (*connect.Response[platformv1.BillingControlState], error) {
	result, err := a.service.SetBillingCollectionPaused(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) ListFleet(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[platformv1.Fleet], error) {
	result, err := a.service.ListFleet(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) CreateAgent(ctx context.Context, req *connect.Request[platformv1.CreateAgentRequest]) (*connect.Response[platformv1.AgentEnrollment], error) {
	result, err := a.service.CreateAgent(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) UpdateAgent(ctx context.Context, req *connect.Request[platformv1.UpdateAgentRequest]) (*connect.Response[platformv1.Agent], error) {
	result, err := a.service.UpdateAgent(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) SetAgentLifecycle(ctx context.Context, req *connect.Request[platformv1.SetAgentLifecycleRequest]) (*connect.Response[platformv1.Agent], error) {
	result, err := a.service.SetAgentLifecycle(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) ListBuilders(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[platformv1.ListBuildersResponse], error) {
	result, err := a.service.ListBuilders(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) SetBuilderDrain(ctx context.Context, req *connect.Request[platformv1.SetBuilderDrainRequest]) (*connect.Response[platformv1.BuilderWorker], error) {
	result, err := a.service.SetBuilderDrain(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) GetBuildScheduler(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[platformv1.BuildSchedulerState], error) {
	result, err := a.service.GetBuildScheduler(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}

func (a connectOpsService) SetBuildSchedulerPaused(ctx context.Context, req *connect.Request[platformv1.SetBuildSchedulerPausedRequest]) (*connect.Response[platformv1.BuildSchedulerState], error) {
	result, err := a.service.SetBuildSchedulerPaused(ctx, req.Msg)
	return connect.NewResponse(result), toConnectError(err)
}
