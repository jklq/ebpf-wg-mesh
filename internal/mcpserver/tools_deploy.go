package mcpserver

import (
	"context"
	"fmt"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *state) latestDeployment(ctx context.Context, serviceID string) (*platformv1.DeploymentRecord, error) {
	resp, err := s.client.ListDeployments(ctx, serviceID, 1)
	if err != nil {
		return nil, err
	}
	if len(resp.GetDeployments()) == 0 {
		return nil, fmt.Errorf("service %s has no deployments yet", serviceID)
	}
	return resp.GetDeployments()[0], nil
}

func (s *state) act(ctx context.Context, serviceID, deploymentID, allocationID string, action platformv1.DeploymentAction) (any, error) {
	if deploymentID == "" {
		latest, err := s.latestDeployment(ctx, serviceID)
		if err != nil {
			return nil, err
		}
		deploymentID = latest.GetId()
	}
	resp, err := s.client.DeploymentAction(ctx, &platformv1.ApplyDeploymentActionRequest{
		ServiceId: serviceID, DeploymentId: deploymentID, Action: action,
		IdempotencyKey: "mcp-" + uuid.NewString(), AllocationId: allocationID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"action": action.String(), "deployment_id": deploymentID, "status": protoJSON(resp)}, nil
}

func parseDeploymentAction(raw string) (platformv1.DeploymentAction, error) {
	switch strings.ToLower(raw) {
	case "restart":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, nil
	case "redeploy", "exact_redeploy":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, nil
	case "rollback":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, nil
	case "cancel":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, nil
	case "remove":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE, nil
	case "retry":
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY, nil
	default:
		if value, ok := platformv1.DeploymentAction_value[raw]; ok {
			return platformv1.DeploymentAction(value), nil
		}
		return 0, fmt.Errorf("action must be restart, redeploy, rollback, cancel, remove, or retry")
	}
}

func (s *state) addDeployTools(server *mcp.Server) {
	type listDeploymentsIn struct {
		ServiceID string `json:"service_id"`
		Limit     int32  `json:"limit,omitempty" jsonschema:"max deployments, newest first (default 20)"`
	}
	add(server, readTool("list_deployments", "List a service's deployments with build status, stages, and action history."), func(ctx context.Context, in listDeploymentsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		return s.client.ListDeployments(ctx, id, limit)
	})

	type deploymentTargetIn struct {
		ServiceID    string `json:"service_id"`
		DeploymentID string `json:"deployment_id,omitempty" jsonschema:"target deployment (default: latest)"`
	}
	add(server, writeTool("redeploy", "Redeploy a service exactly as its (latest) deployment ran: same revision and image."), func(ctx context.Context, in deploymentTargetIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.act(ctx, id, in.DeploymentID, "", platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY)
	})

	type restartIn struct {
		ServiceID    string `json:"service_id"`
		DeploymentID string `json:"deployment_id,omitempty"`
		AllocationID string `json:"allocation_id,omitempty" jsonschema:"restart only this allocation"`
	}
	add(server, writeTool("restart_service", "Restart a service's allocations in place (same deployment)."), func(ctx context.Context, in restartIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.act(ctx, id, in.DeploymentID, in.AllocationID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART)
	})

	type rollbackIn struct {
		ServiceID    string `json:"service_id"`
		DeploymentID string `json:"deployment_id" jsonschema:"earlier deployment to roll back to (see list_deployments)"`
		Confirm      bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("rollback_deployment", "Roll back to an earlier deployment. Destructive: preview first, then confirm:true."), func(ctx context.Context, in rollbackIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		if err := required("deployment_id", in.DeploymentID); err != nil {
			return nil, err
		}
		if !in.Confirm {
			return preview(fmt.Sprintf("roll service %s back to deployment %s", id, in.DeploymentID), nil), nil
		}
		return s.act(ctx, id, in.DeploymentID, "", platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK)
	})

	type actionIn struct {
		ServiceID      string `json:"service_id"`
		DeploymentID   string `json:"deployment_id,omitempty" jsonschema:"default: latest deployment"`
		Action         string `json:"action" jsonschema:"restart, redeploy, rollback, cancel, remove, or retry"`
		AllocationID   string `json:"allocation_id,omitempty"`
		IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"reuse to retry the same action safely"`
		Confirm        bool   `json:"confirm,omitempty" jsonschema:"required for cancel and remove"`
	}
	add(server, destructiveTool("deployment_action", "Apply any deployment action. cancel and remove are destructive and need confirm:true."), func(ctx context.Context, in actionIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		action, err := parseDeploymentAction(in.Action)
		if err != nil {
			return nil, err
		}
		destructive := action == platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL ||
			action == platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE ||
			action == platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK
		if destructive && !in.Confirm {
			return preview(fmt.Sprintf("apply %s to deployment %q of service %s", in.Action, in.DeploymentID, id), nil), nil
		}
		deploymentID := in.DeploymentID
		if deploymentID == "" {
			latest, err := s.latestDeployment(ctx, id)
			if err != nil {
				return nil, err
			}
			deploymentID = latest.GetId()
		}
		key := in.IdempotencyKey
		if key == "" {
			key = "mcp-" + uuid.NewString()
		}
		resp, err := s.client.DeploymentAction(ctx, &platformv1.ApplyDeploymentActionRequest{
			ServiceId: id, DeploymentId: deploymentID, Action: action,
			IdempotencyKey: key, AllocationId: in.AllocationID,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": action.String(), "deployment_id": deploymentID, "status": protoJSON(resp)}, nil
	})

	s.addDiagnoseTool(server)
}

func (s *state) addDiagnoseTool(server *mcp.Server) {
	type diagnoseIn struct {
		ServiceID string `json:"service_id"`
		LogLimit  int32  `json:"log_limit,omitempty" jsonschema:"recent log lines per stream (default 100)"`
	}
	add(server, readTool("diagnose_service", "One-shot diagnosis bundle for a service: config, live allocations, recent deployments, build state/attempts, domains, runtime and build logs, plus likely_issues. Use this first when a deploy fails or a service crashes (Railway-agent style evidence gathering)."), func(ctx context.Context, in diagnoseIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		limit := in.LogLimit
		if limit <= 0 {
			limit = 100
		}
		svc, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		bundle := map[string]any{"service": protoJSON(svc)}
		var issues []string
		if svc.GetPendingChanges() {
			issues = append(issues, "staged changes are not deployed yet (pending_changes); release the environment to apply them")
		}
		if msg := svc.GetPlacementMessage(); msg != "" {
			issues = append(issues, "placement: "+msg)
		}
		if status, err := s.client.GetServiceStatus(ctx, id); err == nil {
			bundle["status"] = protoJSON(status)
			for _, alloc := range status.GetAllocations() {
				restart := alloc.GetRestart()
				if restart.GetCrashLoop() {
					issues = append(issues, fmt.Sprintf("allocation %s is crash-looping (last cause %s, exit %d, signal %d): %s",
						alloc.GetAllocationId(), restart.GetLastCause(), restart.GetLastExitCode(), restart.GetLastSignal(), restart.GetMessage()))
				}
				if !alloc.GetHealthy() && alloc.GetMessage() != "" {
					issues = append(issues, fmt.Sprintf("allocation %s unhealthy in phase %s: %s", alloc.GetAllocationId(), alloc.GetPhase(), alloc.GetMessage()))
				}
			}
		} else {
			bundle["status_error"] = err.Error()
		}
		deployments, err := s.client.ListDeployments(ctx, id, 5)
		if err == nil {
			bundle["recent_deployments"] = protoJSON(deployments)
			if latest := firstOf(deployments.GetDeployments()); latest != nil {
				st := latest.GetStatus()
				switch st.GetState() {
				case platformv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
					platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED,
					platformv1.DeploymentState_DEPLOYMENT_STATE_CANCELLED:
					issues = append(issues, fmt.Sprintf("latest deployment %s is %s (%s: %s)", latest.GetId(), st.GetState(), st.GetReasonCode(), st.GetDetail()))
				}
				if build := latest.GetBuild(); build != nil && build.GetState() == platformv1.BuildState_BUILD_STATE_FAILED {
					issues = append(issues, "latest build failed: "+build.GetFailureReason())
				}
			}
		}
		if build := svc.GetLatestBuild(); build != nil && build.GetBuildId() != "" {
			if attempts, err := s.client.ListBuildAttempts(ctx, id, build.GetBuildId()); err == nil {
				bundle["latest_build_attempts"] = protoJSON(attempts)
			}
			if logs, err := s.client.ListLogs(ctx, &platformv1.ListServiceLogsRequest{
				ServiceId: id, LogType: platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD,
				BuildId: build.GetBuildId(), Limit: limit,
			}); err == nil {
				bundle["build_logs"] = protoJSON(logs)
			}
		}
		if logs, err := s.client.ListLogs(ctx, &platformv1.ListServiceLogsRequest{ServiceId: id, Limit: limit}); err == nil {
			bundle["runtime_logs"] = protoJSON(logs)
			if len(logs.GetGaps()) > 0 {
				issues = append(issues, "log pipeline dropped lines (see runtime_logs.gaps); absence of lines is not proof of inactivity")
			}
		}
		if domains, err := s.client.ListDomains(ctx, id, false); err == nil {
			bundle["domains"] = protoJSON(domains)
			for _, binding := range domains.GetBindings() {
				if binding.GetOwnershipState() == platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED {
					issues = append(issues, fmt.Sprintf("domain %s ownership unverified: %s", binding.GetHostname(), binding.GetOwnershipMessage()))
				}
				if cert := binding.GetCertificate(); cert != nil && cert.GetState() == platformv1.DomainCertificateState_DOMAIN_CERTIFICATE_STATE_FAILED {
					issues = append(issues, fmt.Sprintf("domain %s certificate failed: %s", binding.GetHostname(), cert.GetMessage()))
				}
			}
		}
		if len(issues) == 0 {
			issues = append(issues, "no obvious failure signals; inspect runtime_logs and recent_deployments stages")
		}
		bundle["likely_issues"] = issues
		return bundle, nil
	})
}

func firstOf[T any](items []*T) *T {
	if len(items) == 0 {
		return nil
	}
	return items[0]
}
