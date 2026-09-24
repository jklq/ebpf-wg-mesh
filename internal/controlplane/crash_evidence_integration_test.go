//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCrashEvidenceSurvivesContainerRemoval(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		agentID = "crash-evidence-agent"
		token   = "crash-evidence-bootstrap"
	)
	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		clickhouseURL: clickhouseURL,
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-a", Email: "a@example.com", Projects: []string{"proj-a"}},
		}},
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}},
		withDashboard:   true,
	})

	cert := enrollAgentTLS(t, cp.server, agentID, token)
	stream, streamCancel := openAgentSync(t, cp.server, cert, agentHello(agentID))
	defer streamCancel()
	_ = recvDesiredState(t, stream)

	userA := userContext(t, cp, ctx, "user-a")
	projects, err := cp.dashboard.ListProjects(userA, nil)
	if err != nil || len(projects.GetProjects()) != 1 {
		t.Fatalf("ListProjects: %+v %v", projects, err)
	}
	environments, err := cp.dashboard.ListEnvironments(userA, &platformv1.ListEnvironmentsRequest{ProjectId: projects.GetProjects()[0].GetId()})
	if err != nil || len(environments.GetEnvironments()) == 0 {
		t.Fatalf("ListEnvironments: %+v %v", environments, err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()
	service, err := cp.dashboard.CreateService(userA, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: directImageServiceSpec(pinnedImage("c"), &platformv1.ServiceRuntime{
				CpuMillis:       250,
				MemoryMebibytes: 256,
				Ports:           runtimePortsFromInts([]int32{8080}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := cp.dashboard.ReleaseEnvironment(userA, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	desired := recvDesiredState(t, stream)
	if len(desired.GetServices()) != 1 {
		t.Fatalf("desired services: got %d, want 1", len(desired.GetServices()))
	}
	target := desired.GetServices()[0]
	sessionID := agentHello(agentID).GetSessionId()
	crashLines := []string{"starting worker", "panic: runtime error: index out of range", "goroutine 1 [running]"}
	entries := make([]*agentv1.LogEntry, 0, len(crashLines))
	for i, line := range crashLines {
		entries = append(entries, &agentv1.LogEntry{
			ObservedAt:        timestamppb.Now(),
			EnvironmentId:     target.GetEnvironmentId(),
			ServiceId:         service.GetId(),
			AllocationId:      target.GetAllocationId(),
			Stream:            "stderr",
			RolloutGeneration: target.GetDesiredRolloutGeneration(),
			Sequence:          uint64(i + 1),
			Line:              line,
			LogType:           platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		})
	}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: &agentv1.LogBatch{
		AgentId: agentID,
		Entries: entries,
	}}}); err != nil {
		t.Fatalf("send crash log batch: %v", err)
	}

	crashCondition := func() *agentv1.ServiceCondition {
		return &agentv1.ServiceCondition{
			AllocationId:             target.GetAllocationId(),
			ServiceId:                service.GetId(),
			DesiredSpecRevision:      target.GetDesiredSpecRevision(),
			AppliedSpecRevision:      target.GetDesiredSpecRevision(),
			DesiredRolloutGeneration: target.GetDesiredRolloutGeneration(),
			AppliedRolloutGeneration: target.GetDesiredRolloutGeneration(),
			Phase:                    "CrashLoop",
			Message:                  "crash loop after OOM kill (5 restarts, policy on-failure)",
			AllocationIpv4:           target.GetPrivateIpv4(),
			AllocationIpv6:           target.GetPrivateIpv6(),
			Healthy:                  false,
			Restart: &platformv1.RestartObservation{
				RestartCount:             5,
				CrashLoop:                true,
				LastCause:                platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
				LastExitCode:             137,
				Message:                  "crash loop after OOM kill (5 restarts, policy on-failure)",
				AppliedRolloutGeneration: target.GetDesiredRolloutGeneration(),
				WindowStartedAt:          timestamppb.New(time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)),
			},
		}
	}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: &agentv1.StatusReport{
		AgentId: agentID, SessionId: sessionID, ObservationSequence: 1,
		AuthorityEpoch: desired.GetAuthorityEpoch(), ReconciliationCursor: desired.GetReconciliationCursor(),
		Services: []*agentv1.ServiceCondition{crashCondition()},
	}}}); err != nil {
		t.Fatalf("send crash-loop report: %v", err)
	}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: &agentv1.StatusReport{
		AgentId: agentID, SessionId: sessionID, ObservationSequence: 2,
		AuthorityEpoch: desired.GetAuthorityEpoch(), ReconciliationCursor: desired.GetReconciliationCursor(),
		Services: []*agentv1.ServiceCondition{crashCondition()},
	}}}); err != nil {
		t.Fatalf("send post-removal crash-loop report: %v", err)
	}

	var status *platformv1.ServiceStatus
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.GetServiceStatus(userA, &platformv1.GetServiceStatusRequest{ServiceId: service.GetId()})
		if err != nil {
			return false, err
		}
		status = resp
		alloc := resp.GetAllocation()
		if alloc == nil && len(resp.GetAllocations()) > 0 {
			alloc = resp.GetAllocations()[0]
		}
		if alloc == nil || !alloc.GetRestart().GetCrashLoop() {
			return false, nil
		}
		return alloc.GetRestart().GetRestartCount() == 5 &&
			alloc.GetRestart().GetLastExitCode() == 137 &&
			alloc.GetRestart().GetLastCause() == platformv1.RestartCause_RESTART_CAUSE_OOM_KILL, nil
	}); err != nil {
		t.Fatalf("crash evidence did not survive container removal: %v (last status %+v)", err, status)
	}
	if got := status.GetService().GetLatestDeployment().GetState(); got != platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED && got != platformv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Fatalf("deployment state = %s, want CRASHED or FAILED", got)
	}

	var tail []string
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userA, &platformv1.ListServiceLogsRequest{
			ServiceId:    service.GetId(),
			AllocationId: target.GetAllocationId(),
			Limit:        30,
			LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		})
		if err != nil {
			return false, err
		}
		tail = nil
		for _, line := range resp.GetLines() {
			tail = append(tail, line.GetLine())
		}
		for _, want := range crashLines {
			found := false
			for _, got := range tail {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatalf("crash log tail did not survive container removal: %v (tail %q)", err, tail)
	}

	// Both reports carried the same crash-loop observation. The
	// platform event must collapse into one row instead of
	// accumulating per report.
	var events int
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userA, &platformv1.ListServiceLogsRequest{
			ServiceId: service.GetId(),
			Limit:     100,
			LogType:   platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY,
		})
		if err != nil {
			return false, err
		}
		events = 0
		for _, line := range resp.GetLines() {
			if line.GetEvent() == "allocation.crash_loop" {
				events++
			}
		}
		return events >= 1, nil
	}); err != nil {
		t.Fatalf("crash-loop event was not emitted: %v", err)
	}
	// Let any duplicate from the second report settle, then re-count.
	time.Sleep(3 * time.Second)
	resp, err := cp.dashboard.ListServiceLogs(userA, &platformv1.ListServiceLogsRequest{
		ServiceId: service.GetId(),
		Limit:     100,
		LogType:   platformv1.ServiceLogType_SERVICE_LOG_TYPE_DEPLOY,
	})
	if err != nil {
		t.Fatalf("ListServiceLogs (deploy): %v", err)
	}
	events = 0
	for _, line := range resp.GetLines() {
		if line.GetEvent() == "allocation.crash_loop" {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("resent crash-loop observation produced %d event rows, want 1", events)
	}
}
