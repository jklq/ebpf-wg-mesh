//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestServiceLogsRuntimeIngestQueryAndIsolation(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		agentID = "logs-agent"
		token   = "logs-bootstrap"
	)
	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		clickhouseURL: clickhouseURL,
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-a", Email: "a@example.com", Projects: []string{"proj-a"}},
			{ID: "user-b", Email: "b@example.com", Projects: []string{"proj-b"}},
		}},
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}},
		withDashboard:   true,
	})

	cert := enrollAgentTLS(t, cp.server, agentID, token)
	stream, streamCancel := openAgentSync(t, cp.server, cert, agentHello(agentID))
	defer streamCancel()
	_ = recvDesiredState(t, stream)

	store := cp.server.store
	projectsA, err := store.listProjects(ctx, "user-a")
	if err != nil || len(projectsA) != 1 {
		t.Fatalf("projects A: %v", err)
	}
	projectsB, err := store.listProjects(ctx, "user-b")
	if err != nil || len(projectsB) != 1 {
		t.Fatalf("projects B: %v", err)
	}
	svcA, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projectsA[0].ID), "web-a", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	svcB, err := createService(ctx, store, "user-b", productionEnvironmentID(t, store, projectsB[0].ID), "web-b", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	allocA := allocationIDForService(t, store, svcA.ID)
	allocB := allocationIDForService(t, store, svcB.ID)
	line := "runtime-a-" + time.Now().UTC().Format("150405.000")

	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: &agentv1.LogBatch{
		AgentId: agentID,
		Entries: []*agentv1.LogEntry{{
			ObservedAt:        timestamppb.Now(),
			EnvironmentId:     svcA.EnvironmentID,
			ServiceId:         svcA.ID,
			AllocationId:      allocA,
			Stream:            "stdout",
			RolloutGeneration: 1,
			Sequence:          1,
			Line:              line,
			LogType:           platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		}},
	}}}); err != nil {
		t.Fatalf("send runtime batch: %v", err)
	}

	userA := userContext(t, ctx, "user-a")
	var got *platformv1.ServiceLogLine
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userA, &platformv1.ListServiceLogsRequest{ServiceId: svcA.ID})
		if err != nil {
			return false, err
		}
		for _, item := range resp.GetLines() {
			if item.GetLine() == line {
				got = item
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("owner A did not see runtime line: %v", err)
	}
	if got.GetLogType() != platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME ||
		got.GetAllocationId() != allocA || got.GetAgentId() != agentID || got.GetRolloutGeneration() != 1 {
		t.Fatalf("runtime line metadata: %+v", got)
	}

	userB := userContext(t, ctx, "user-b")
	respB, err := cp.dashboard.ListServiceLogs(userB, &platformv1.ListServiceLogsRequest{ServiceId: svcA.ID})
	if status.Code(err) != codes.NotFound {
		if err == nil {
			for _, item := range respB.GetLines() {
				if item.GetLine() == line {
					t.Fatal("owner B saw tenant A's runtime line")
				}
			}
		} else {
			t.Fatalf("owner B ListServiceLogs(A): %v", err)
		}
	}

	spoof := &agentv1.LogBatch{
		AgentId: agentID,
		Entries: []*agentv1.LogEntry{{
			ObservedAt:    timestamppb.Now(),
			EnvironmentId: svcB.EnvironmentID,
			ServiceId:     svcB.ID,
			AllocationId:  allocA,
			Stream:        "stdout",
			Line:          "spoof-cross-tenant",
			LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		}},
	}
	if err := store.validateAgentLogBatch(ctx, agentID, spoof); err == nil {
		t.Fatal("validateAgentLogBatch accepted a batch that claimed B's service with A's allocation")
	}
	foreignAlloc := &agentv1.LogBatch{
		AgentId: agentID,
		Entries: []*agentv1.LogEntry{{
			ObservedAt:    timestamppb.Now(),
			EnvironmentId: svcA.EnvironmentID,
			ServiceId:     svcA.ID,
			AllocationId:  "alloc-not-on-this-agent",
			Line:          "spoof-unassigned",
		}},
	}
	if err := store.validateAgentLogBatch(ctx, agentID, foreignAlloc); err == nil {
		t.Fatal("validateAgentLogBatch accepted an allocation not assigned to the agent")
	}

	for _, userID := range []string{"user-a", "user-b"} {
		resp, err := cp.dashboard.ListServiceLogs(userContext(t, ctx, userID), &platformv1.ListServiceLogsRequest{ServiceId: map[string]string{"user-a": svcA.ID, "user-b": svcB.ID}[userID]})
		if err != nil {
			t.Fatalf("ListServiceLogs(%s): %v", userID, err)
		}
		for _, item := range resp.GetLines() {
			if strings.Contains(item.GetLine(), "spoof-") {
				t.Fatalf("spoofed line appeared for %s: %+v", userID, item)
			}
		}
	}
	_ = allocB
}

func TestServiceLogsMixedAuthorsAndDisabledStoreFailClosed(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		agentID = "logs-mixed-agent"
		token   = "logs-mixed-bootstrap"
	)
	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		clickhouseURL: clickhouseURL,
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-a", Email: "a@example.com", Projects: []string{"proj-a"}},
			{ID: "user-b", Email: "b@example.com", Projects: []string{"proj-b"}},
		}},
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}},
		withDashboard:   true,
	})
	store := cp.server.store
	if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-a")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	service, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	allocID := allocationIDForService(t, store, service.ID)
	suffix := time.Now().UTC().Format("150405.000")
	buildLine := "build-line-" + suffix
	deployLine := "deploy-line-" + suffix
	runtimeLine := "runtime-line-" + suffix

	cp.server.logEmitter.EmitBuild(ctx, service, buildRunRecord{ID: "build-" + suffix}, StageBuild, buildLine)
	cp.server.logEmitter.EmitDeploy(ctx, service, allocID, "build-"+suffix, StageDeploy, deployLine)

	cert := enrollAgentTLS(t, cp.server, agentID, token)
	stream, streamCancel := openAgentSync(t, cp.server, cert, agentHello(agentID))
	defer streamCancel()
	_ = recvDesiredState(t, stream)
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: &agentv1.LogBatch{
		AgentId: agentID,
		Entries: []*agentv1.LogEntry{{
			ObservedAt:        timestamppb.Now(),
			EnvironmentId:     service.EnvironmentID,
			ServiceId:         service.ID,
			AllocationId:      allocID,
			Stream:            "stdout",
			RolloutGeneration: service.RolloutGeneration,
			Sequence:          9,
			Line:              runtimeLine,
			LogType:           platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		}},
	}}}); err != nil {
		t.Fatalf("runtime batch: %v", err)
	}

	userA := userContext(t, ctx, "user-a")
	var lines []*platformv1.ServiceLogLine
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userA, &platformv1.ListServiceLogsRequest{ServiceId: service.ID})
		if err != nil {
			return false, err
		}
		lines = resp.GetLines()
		return logLineExists(lines, buildLine, platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD) &&
			logLineExists(lines, deployLine, platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY) &&
			logLineExists(lines, runtimeLine, platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME), nil
	}); err != nil {
		t.Fatalf("mixed authors did not meet in ClickHouse: %v", err)
	}
	for _, item := range lines {
		switch item.GetLine() {
		case buildLine:
			if item.GetStage() != StageBuild {
				t.Fatalf("build stage = %q", item.GetStage())
			}
		case deployLine:
			if item.GetStage() != StageDeploy {
				t.Fatalf("deploy stage = %q", item.GetStage())
			}
		}
	}

	_, err = cp.dashboard.ListServiceLogs(userContext(t, ctx, "user-b"), &platformv1.ListServiceLogsRequest{ServiceId: service.ID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("membership bypass: %v", err)
	}

	disabled := startSystemControlPlane(t, systemControlPlaneOptions{
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-a", Email: "a@example.com", Projects: []string{"disabled"}},
		}},
		withDashboard: true,
	})
	if _, err := upsertTestAgent(t, disabled.server.store, ctx, agentHello("disabled-agent")); err != nil {
		t.Fatal(err)
	}
	disabledProjects, err := disabled.server.store.listProjects(ctx, "user-a")
	if err != nil || len(disabledProjects) != 1 {
		t.Fatalf("disabled projects: %v", err)
	}
	disabledSvc, err := createService(ctx, disabled.server.store, "user-a", productionEnvironmentID(t, disabled.server.store, disabledProjects[0].ID), "web", serviceSpec(), "disabled-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, err = disabled.dashboard.ListServiceLogs(userContext(t, ctx, "user-a"), &platformv1.ListServiceLogsRequest{ServiceId: disabledSvc.ID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled log store returned %v, want FailedPrecondition", err)
	}
}

func allocationIDForService(t *testing.T, store *Store, serviceID string) string {
	t.Helper()
	var id string
	if err := store.db.QueryRowContext(context.Background(), `SELECT id FROM allocations WHERE service_id = $1`, serviceID).Scan(&id); err != nil {
		t.Fatalf("allocation for %s: %v", serviceID, err)
	}
	return id
}

func logLineExists(lines []*platformv1.ServiceLogLine, want string, logType platformv1.ServiceLogType) bool {
	for _, line := range lines {
		if line.GetLine() == want && line.GetLogType() == logType {
			return true
		}
	}
	return false
}
