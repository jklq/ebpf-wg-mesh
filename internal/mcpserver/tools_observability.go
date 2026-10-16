package mcpserver

import (
	"context"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type logsIn struct {
	ServiceID    string `json:"service_id"`
	LogType      string `json:"log_type,omitempty" jsonschema:"runtime (default), build, deploy, http, or network"`
	Limit        int32  `json:"limit,omitempty" jsonschema:"max lines, newest first (default 100)"`
	Search       string `json:"search,omitempty" jsonschema:"substring filter"`
	BuildID      string `json:"build_id,omitempty" jsonschema:"for build logs; default latest build in get_build_logs"`
	AllocationID string `json:"allocation_id,omitempty"`
	StartTime    string `json:"start_time,omitempty" jsonschema:"RFC3339"`
	EndTime      string `json:"end_time,omitempty" jsonschema:"RFC3339"`
	PageToken    string `json:"page_token,omitempty" jsonschema:"older-page cursor from a previous response"`
}

func parseLogType(raw string) (platformv1.ServiceLogType, error) {
	switch raw {
	case "", "runtime":
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME, nil
	case "build":
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD, nil
	case "deploy":
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY, nil
	case "http":
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_HTTP, nil
	case "network":
		return platformv1.ServiceLogType_SERVICE_LOG_TYPE_NETWORK, nil
	default:
		if value, ok := platformv1.ServiceLogType_value[raw]; ok {
			return platformv1.ServiceLogType(value), nil
		}
		return 0, fmt.Errorf("log_type must be runtime, build, deploy, http, or network")
	}
}

func (s *state) logsRequest(serviceID string, in logsIn, logType platformv1.ServiceLogType) (*platformv1.ListServiceLogsRequest, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	req := &platformv1.ListServiceLogsRequest{
		ServiceId: serviceID, LogType: logType, Limit: limit,
		Search: in.Search, BuildId: in.BuildID, AllocationId: in.AllocationID, PageToken: in.PageToken,
	}
	if in.StartTime != "" {
		parsed, err := time.Parse(time.RFC3339, in.StartTime)
		if err != nil {
			return nil, fmt.Errorf("start_time must be RFC3339: %w", err)
		}
		req.StartTime = timestamppb.New(parsed)
	}
	if in.EndTime != "" {
		parsed, err := time.Parse(time.RFC3339, in.EndTime)
		if err != nil {
			return nil, fmt.Errorf("end_time must be RFC3339: %w", err)
		}
		req.EndTime = timestamppb.New(parsed)
	}
	return req, nil
}

func (s *state) addObservabilityTools(server *mcp.Server) {
	add(server, readTool("get_logs", "Read service logs (runtime/build/deploy/http/network), newest first, with explicit gap markers where lines were dropped and page tokens for older lines."), func(ctx context.Context, in logsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		logType, err := parseLogType(in.LogType)
		if err != nil {
			return nil, err
		}
		req, err := s.logsRequest(id, in, logType)
		if err != nil {
			return nil, err
		}
		return s.client.ListLogs(ctx, req)
	})

	type buildLogsIn struct {
		ServiceID string `json:"service_id"`
		BuildID   string `json:"build_id,omitempty" jsonschema:"default: latest build"`
		Limit     int32  `json:"limit,omitempty"`
		PageToken string `json:"page_token,omitempty"`
	}
	add(server, readTool("get_build_logs", "Read build logs (Railpack/Dockerfile output) for a build; defaults to the latest build."), func(ctx context.Context, in buildLogsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		buildID := in.BuildID
		if buildID == "" {
			svc, err := s.client.GetService(ctx, id)
			if err != nil {
				return nil, err
			}
			buildID = svc.GetLatestBuild().GetBuildId()
			if buildID == "" {
				return nil, fmt.Errorf("service has no builds (direct images do not build)")
			}
		}
		req, err := s.logsRequest(id, logsIn{BuildID: buildID, Limit: in.Limit, PageToken: in.PageToken}, platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.ListLogs(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"build_id": buildID, "logs": protoJSON(resp)}, nil
	})

	type attemptsIn struct {
		ServiceID string `json:"service_id"`
		BuildID   string `json:"build_id" jsonschema:"build ID from list_deployments or get_service latest_build"`
	}
	add(server, readTool("list_build_attempts", "List lease attempts of one build (worker losses, retries, timeouts)."), func(ctx context.Context, in attemptsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.ListBuildAttempts(ctx, id, in.BuildID)
	})

	type artifactsIn struct {
		ServiceID string `json:"service_id"`
		Limit     int32  `json:"limit,omitempty"`
	}
	add(server, readTool("list_service_artifacts", "List immutable deploy-by-digest image artifacts of a service."), func(ctx context.Context, in artifactsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.ListArtifacts(ctx, id, in.Limit)
	})

	type httpLogsIn struct {
		ServiceID string `json:"service_id"`
		Limit     int32  `json:"limit,omitempty"`
		Search    string `json:"search,omitempty"`
		StartTime string `json:"start_time,omitempty"`
		EndTime   string `json:"end_time,omitempty"`
		PageToken string `json:"page_token,omitempty"`
	}
	add(server, readTool("http_logs", "HTTP request logs through ingress for a service (status/method/path live in line attributes). Use search to filter, e.g. a status code."), func(ctx context.Context, in httpLogsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		req, err := s.logsRequest(id, logsIn{Limit: in.Limit, Search: in.Search, StartTime: in.StartTime, EndTime: in.EndTime, PageToken: in.PageToken}, platformv1.ServiceLogType_SERVICE_LOG_TYPE_HTTP)
		if err != nil {
			return nil, err
		}
		return s.client.ListLogs(ctx, req)
	})

	add(server, readTool("list_fleet_agents", "List fleet agents with capacity and health (platform operators only; account-scoped token)."), func(ctx context.Context, _ empty) (any, error) {
		return s.client.ListAgents(ctx)
	})
}
