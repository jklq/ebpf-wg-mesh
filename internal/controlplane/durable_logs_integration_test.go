//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/logpipeline"
	"ebof-wg-mesh/internal/testutil"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func durableLogEntry(envID, serviceID, allocID, lineID, line string, observedAt time.Time, seq uint64) *agentv1.LogEntry {
	return &agentv1.LogEntry{
		ObservedAt:    timestamppb.New(observedAt),
		EnvironmentId: envID,
		ServiceId:     serviceID,
		AllocationId:  allocID,
		Stream:        "stdout",
		Sequence:      seq,
		Line:          line,
		LineId:        lineID,
		LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
	}
}

func sendDurableBatch(t *testing.T, stream agentv1.AgentControl_SyncClient, agentID string, entries []*agentv1.LogEntry, drops ...*platformv1.LogDropSummary) {
	t.Helper()
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: &agentv1.LogBatch{
		AgentId: agentID,
		Entries: entries,
		Drops:   drops,
	}}}); err != nil {
		t.Fatalf("send log batch: %v", err)
	}
}

func pollServiceLogLines(t *testing.T, ctx context.Context, cp *systemControlPlane, userID, serviceID string, want int, timeout time.Duration) []*platformv1.ServiceLogLine {
	t.Helper()
	var lines []*platformv1.ServiceLogLine
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: timeout, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userContext(t, cp, ctx, userID), &platformv1.ListServiceLogsRequest{ServiceId: serviceID, Limit: 5000})
		if err != nil {
			return false, err
		}
		lines = resp.GetLines()
		return len(lines) >= want, nil
	}); err != nil {
		t.Fatalf("wait for %d log lines: %v", want, err)
	}
	return lines
}

func TestDurableLogsRetryDedupTruncationAndPagination(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		agentID = "durable-agent"
		token   = "durable-bootstrap"
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

	store := cp.server.store
	projects, err := store.catalog.listProjects(ctx, testUser("user-a"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects: %v", err)
	}
	svc, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	allocID := allocationIDForService(t, store, svc.ID)
	base := time.Now().UTC().Truncate(time.Second)

	// Retry the identical batch: stable line IDs must collapse into
	// one copy of each line.
	dedupEntries := []*agentv1.LogEntry{
		durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "retry-line-1", "dedup-marker-one", base, 1),
		durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "retry-line-2", "dedup-marker-two", base.Add(time.Second), 2),
	}
	sendDurableBatch(t, stream, agentID, dedupEntries)
	sendDurableBatch(t, stream, agentID, dedupEntries)

	// An oversized line keeps its exact prefix and reports truncation.
	huge := strings.Repeat("z", logpipeline.MaxLogLineBytes+500)
	sendDurableBatch(t, stream, agentID, []*agentv1.LogEntry{
		durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "huge-line-1", huge, base.Add(2*time.Second), 3),
	})

	// Paged lines land an hour later so time ordering is unambiguous.
	var paged []*agentv1.LogEntry
	for i := 0; i < 5; i++ {
		paged = append(paged, durableLogEntry(svc.EnvironmentID, svc.ID, allocID,
			fmt.Sprintf("page-line-%d", i), fmt.Sprintf("page-marker-%d", i), base.Add(time.Hour+time.Duration(i)*time.Second), uint64(10+i)))
	}
	sendDurableBatch(t, stream, agentID, paged)

	lines := pollServiceLogLines(t, ctx, cp, "user-a", svc.ID, 8, 30*time.Second)
	var dedup, hugeCount int
	var hugeLine *platformv1.ServiceLogLine
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line.GetLine(), "dedup-marker"):
			dedup++
		case strings.HasPrefix(line.GetLine(), "zzz"):
			hugeCount++
			hugeLine = line
		}
	}
	if dedup != 2 {
		t.Fatalf("retried batch produced %d dedup lines, want 2", dedup)
	}
	if hugeCount != 1 {
		t.Fatalf("oversized line produced %d rows, want 1", hugeCount)
	}
	if len(hugeLine.GetLine()) != logpipeline.MaxLogLineBytes || !hugeLine.GetTruncated() {
		t.Fatalf("oversized line: len=%d truncated=%v", len(hugeLine.GetLine()), hugeLine.GetTruncated())
	}
	if strings.Trim(hugeLine.GetLine(), "z") != "" {
		t.Fatal("truncated prefix is not byte-exact")
	}

	// Cursor pagination walks the whole range oldest-first without
	// duplicates or omissions.
	userCtx := userContext(t, cp, ctx, "user-a")
	var (
		pagedIDs  []string
		pageToken string
		pages     int
	)
	for {
		resp, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svc.ID, Limit: 3, PageToken: pageToken})
		if err != nil {
			t.Fatalf("paged list: %v", err)
		}
		pages++
		for _, line := range resp.GetLines() {
			pagedIDs = append(pagedIDs, line.GetLineId())
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		pageToken = resp.GetNextPageToken()
		if pages > 10 {
			t.Fatal("pagination did not exhaust")
		}
	}
	if len(pagedIDs) != 8 {
		t.Fatalf("pagination returned %d lines, want 8", len(pagedIDs))
	}
	seen := make(map[string]struct{}, len(pagedIDs))
	for _, id := range pagedIDs {
		if _, dup := seen[id]; dup {
			t.Fatalf("pagination duplicated line %q", id)
		}
		seen[id] = struct{}{}
	}
	// The last five in range order are the page markers, in order.
	resp, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svc.ID, Limit: 5000})
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	got := resp.GetLines()
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("page-marker-%d", i)
		if got[len(got)-5+i].GetLine() != want {
			t.Fatalf("range order wrong at tail %d: %q", i, got[len(got)-5+i].GetLine())
		}
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1].GetObservedAt().AsTime(), got[i].GetObservedAt().AsTime()
		if cur.Before(prev) {
			t.Fatal("lines are not oldest-first")
		}
	}

	if _, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svc.ID, PageToken: "bogus"}); status.Code(err) != codes.Internal {
		t.Fatalf("bogus page token returned %v, want Internal", err)
	}
}

func TestDurableLogsProducerAndIngestGapsSurfaceInReads(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		agentID = "gap-agent"
		token   = "gap-bootstrap"
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

	store := cp.server.store
	projects, err := store.catalog.listProjects(ctx, testUser("user-a"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects: %v", err)
	}
	svc, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	allocID := allocationIDForService(t, store, svc.ID)
	now := time.Now().UTC().Truncate(time.Second)

	// A producer drop report becomes an explicit read gap.
	sendDurableBatch(t, stream, agentID,
		[]*agentv1.LogEntry{durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "gap-line-1", "after-gap", now, 1)},
		&platformv1.LogDropSummary{
			ServiceId:    svc.ID,
			AllocationId: allocID,
			LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
			Stream:       "stdout",
			DroppedCount: 7,
			Reason:       logpipeline.ReasonRateLimited,
			WindowStart:  timestamppb.New(now.Add(-time.Minute)),
			WindowEnd:    timestamppb.New(now),
		},
	)
	userCtx := userContext(t, cp, ctx, "user-a")
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svc.ID})
		if err != nil {
			return false, err
		}
		for _, gap := range resp.GetGaps() {
			if gap.GetDroppedCount() == 7 && gap.GetReason() == logpipeline.ReasonRateLimited && gap.GetAllocationId() == allocID {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("producer gap did not surface in reads: %v", err)
	}

	// An abusive batch past the ingest cap sheds its tail with an
	// explicit gap instead of choking ClickHouse.
	var flood []*agentv1.LogEntry
	for i := 0; i < 2005; i++ {
		seq := uint64(1000 + i)
		flood = append(flood, durableLogEntry(svc.EnvironmentID, svc.ID, allocID, fmt.Sprintf("flood-%d", i), "flood-line", now.Add(time.Duration(i)*time.Millisecond), seq))
	}
	sendDurableBatch(t, stream, agentID, flood)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svc.ID})
		if err != nil {
			return false, err
		}
		for _, gap := range resp.GetGaps() {
			if gap.GetDroppedCount() == 5 && gap.GetReason() == logpipeline.ReasonIngestOverflow {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("ingest-overflow gap did not surface in reads: %v", err)
	}
}

func TestDurableLogsRetentionAndSafeDeletion(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const (
		agentID = "retention-agent"
		token   = "retention-bootstrap"
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
	projectsA, err := store.catalog.listProjects(ctx, testUser("user-a"), false)
	if err != nil || len(projectsA) != 1 {
		t.Fatalf("projects A: %v", err)
	}
	projectsB, err := store.catalog.listProjects(ctx, testUser("user-b"), false)
	if err != nil || len(projectsB) != 1 {
		t.Fatalf("projects B: %v", err)
	}
	projA, projB := projectsA[0], projectsB[0]
	svcA, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projA.ID), "web-a", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	svcB, err := createService(ctx, store, "user-b", productionEnvironmentID(t, store, projB.ID), "web-b", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	allocA := allocationIDForService(t, store, svcA.ID)
	allocB := allocationIDForService(t, store, svcB.ID)

	// Tenant retention policy: set, reset, bounds, and authorization.
	userA := userContext(t, cp, ctx, "user-a")
	updated, err := cp.dashboard.UpdateProjectLogRetention(userA, &platformv1.UpdateProjectLogRetentionRequest{ProjectId: projA.ID, LogRetentionDays: 3})
	if err != nil {
		t.Fatalf("UpdateProjectLogRetention: %v", err)
	}
	if updated.GetLogRetentionDays() != 3 {
		t.Fatalf("retention = %d, want 3", updated.GetLogRetentionDays())
	}
	for _, days := range []int32{-1, 91} {
		if _, err := cp.dashboard.UpdateProjectLogRetention(userA, &platformv1.UpdateProjectLogRetentionRequest{ProjectId: projA.ID, LogRetentionDays: days}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("retention %d returned %v, want InvalidArgument", days, err)
		}
	}
	if _, err := cp.dashboard.UpdateProjectLogRetention(userContext(t, cp, ctx, "user-b"), &platformv1.UpdateProjectLogRetentionRequest{ProjectId: projA.ID, LogRetentionDays: 5}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant retention update returned %v, want PermissionDenied", err)
	}
	if _, err := cp.dashboard.UpdateProjectLogRetention(userA, &platformv1.UpdateProjectLogRetentionRequest{ProjectId: projA.ID, LogRetentionDays: 0}); err != nil {
		t.Fatalf("reset retention: %v", err)
	}
	if _, err := cp.dashboard.UpdateProjectLogRetention(userA, &platformv1.UpdateProjectLogRetentionRequest{ProjectId: projA.ID, LogRetentionDays: 3}); err != nil {
		t.Fatalf("restore retention: %v", err)
	}

	// The policy lands on the row expiry and tenant attribution.
	now := time.Now().UTC().Truncate(time.Second)
	retentionMarker := "retention-marker-" + now.Format("150405")
	sendDurableBatch(t, stream, agentID, []*agentv1.LogEntry{
		durableLogEntry(svcA.EnvironmentID, svcA.ID, allocA, "retention-line-1", retentionMarker, now, 1),
	})
	pollServiceLogLines(t, ctx, cp, "user-a", svcA.ID, 1, 30*time.Second)
	chdb, err := sql.Open("clickhouse", clickhouseURL)
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer chdb.Close()
	var expiresAt time.Time
	var rowProjectID string
	if err := chdb.QueryRowContext(ctx, `SELECT expires_at, project_id FROM service_logs FINAL WHERE service_id = ? AND line = ?`, svcA.ID, retentionMarker).Scan(&expiresAt, &rowProjectID); err != nil {
		t.Fatalf("query row expiry: %v", err)
	}
	if rowProjectID != projA.ID {
		t.Fatalf("row project = %q, want %q", rowProjectID, projA.ID)
	}
	wantExpiry := time.Now().Add(3 * 24 * time.Hour)
	if expiresAt.Before(wantExpiry.Add(-10*time.Minute)) || expiresAt.After(wantExpiry.Add(10*time.Minute)) {
		t.Fatalf("row expires_at = %v, want ~%v", expiresAt, wantExpiry)
	}

	// Gap rows follow the same project retention as lines.
	sendDurableBatch(t, stream, agentID, nil,
		&platformv1.LogDropSummary{
			ServiceId:    svcA.ID,
			AllocationId: allocA,
			LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
			Stream:       "stdout",
			DroppedCount: 2,
			Reason:       logpipeline.ReasonRateLimited,
			WindowStart:  timestamppb.New(now.Add(-time.Minute)),
			WindowEnd:    timestamppb.New(now),
		},
	)
	var gapExpires time.Time
	var gapProjectID string
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		err := chdb.QueryRowContext(ctx, `SELECT expires_at, project_id FROM service_log_gaps FINAL WHERE service_id = ? AND dropped_count = 2`, svcA.ID).Scan(&gapExpires, &gapProjectID)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		t.Fatalf("query gap expiry: %v", err)
	}
	if gapProjectID != projA.ID {
		t.Fatalf("gap project = %q, want %q", gapProjectID, projA.ID)
	}
	if gapExpires.Before(wantExpiry.Add(-10*time.Minute)) || gapExpires.After(wantExpiry.Add(10*time.Minute)) {
		t.Fatalf("gap expires_at = %v, want ~%v", gapExpires, wantExpiry)
	}

	// Tenant B's logs must survive tenant A's deletion.
	keepMarker := "keep-marker-" + now.Format("150405")
	sendDurableBatch(t, stream, agentID, []*agentv1.LogEntry{
		durableLogEntry(svcB.EnvironmentID, svcB.ID, allocB, "keep-line-1", keepMarker, now, 1),
	})
	pollServiceLogLines(t, ctx, cp, "user-b", svcB.ID, 1, 30*time.Second)

	if _, err := cp.dashboard.DeleteProject(userA, &platformv1.DeleteProjectRequest{ProjectId: projA.ID, ConfirmationName: projA.Name}); err != nil {
		t.Fatalf("delete project A: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET delete_expires_at = now() - INTERVAL '1 second' WHERE id = $1`, projA.ID); err != nil {
		t.Fatalf("force project expiry: %v", err)
	}
	var purged []string
	gc := NewDeletionGC(store, nil, nil, time.Second)
	gc.SetLogPurgeHook(func(ctx context.Context, projectID string) error {
		purged = append(purged, projectID)
		return cp.server.logStore.PurgeProjectLogs(ctx, projectID)
	})
	stats, err := gc.CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if stats.ByKind[ExpiredDeletionProject] != 1 {
		t.Fatalf("expected 1 collected project, got %+v", stats.ByKind)
	}
	if len(purged) != 1 || purged[0] != projA.ID {
		t.Fatalf("purge hook calls = %v, want [%s]", purged, projA.ID)
	}
	var remainingA int
	if err := chdb.QueryRowContext(ctx, `SELECT count() FROM service_logs WHERE project_id = ?`, projA.ID).Scan(&remainingA); err != nil {
		t.Fatalf("count purged rows: %v", err)
	}
	if remainingA != 0 {
		t.Fatalf("purged project still has %d log rows", remainingA)
	}
	respB, err := cp.dashboard.ListServiceLogs(userContext(t, cp, ctx, "user-b"), &platformv1.ListServiceLogsRequest{ServiceId: svcB.ID})
	if err != nil {
		t.Fatalf("list B after purge: %v", err)
	}
	found := false
	for _, line := range respB.GetLines() {
		if line.GetLine() == keepMarker {
			found = true
		}
	}
	if !found {
		t.Fatal("tenant B's logs did not survive tenant A's purge")
	}
	_ = projB
}

func TestDurableLogsSurviveClickHouseOutage(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	containerName := fmt.Sprintf("logs-outage-%d", time.Now().UnixNano())
	port := availableLocalPort(t)
	managed, err := localteststack.StartManagedClickHouse(ctx, localteststack.LocalClickHouseConfig{
		ContainerName: containerName,
		NativePort:    port,
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		t.Skipf("ClickHouse cannot be started: %v", err)
	}
	t.Cleanup(func() {
		if err := managed.Close(); err != nil {
			t.Errorf("close ClickHouse: %v", err)
		}
	})
	const (
		agentID = "outage-agent"
		token   = "outage-bootstrap"
	)
	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		clickhouseURL: managed.URL(),
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-a", Email: "a@example.com", Projects: []string{"proj-a"}},
		}},
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}},
		withDashboard:   true,
	})
	cert := enrollAgentTLS(t, cp.server, agentID, token)
	hello := agentHello(agentID)
	stream, streamCancel := openAgentSync(t, cp.server, cert, hello)
	defer streamCancel()
	_ = recvDesiredState(t, stream)

	store := cp.server.store
	projects, err := store.catalog.listProjects(ctx, testUser("user-a"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects: %v", err)
	}
	svc, err := createService(ctx, store, "user-a", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), agentID)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	allocID := allocationIDForService(t, store, svc.ID)
	now := time.Now().UTC().Truncate(time.Second)

	beforeMarker := "outage-before-" + now.Format("150405")
	sendDurableBatch(t, stream, agentID, []*agentv1.LogEntry{
		durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "outage-line-1", beforeMarker, now, 1),
	})
	pollServiceLogLines(t, ctx, cp, "user-a", svc.ID, 1, 30*time.Second)

	// Stop the backend: the Sync stream must stay healthy and ingest
	// must not fail the stream.
	if err := managed.Stop(ctx); err != nil {
		t.Fatalf("stop clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = managed.Start(context.Background())
	})
	duringMarker := "outage-during-" + now.Format("150405")
	sendDurableBatch(t, stream, agentID, []*agentv1.LogEntry{
		durableLogEntry(svc.EnvironmentID, svc.ID, allocID, "outage-line-2", duringMarker, now.Add(time.Second), 2),
	})
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Heartbeat{Heartbeat: &agentv1.AgentHeartbeat{
		AgentId:   agentID,
		SessionId: hello.GetSessionId(),
	}}}); err != nil {
		t.Fatalf("heartbeat during ClickHouse outage failed: %v", err)
	}

	// Restart: the queued line flushes exactly once.
	if err := managed.Start(ctx); err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 90 * time.Second, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		return cp.server.logStore.Ready(ctx), nil
	}); err != nil {
		t.Fatalf("clickhouse did not recover: %v", err)
	}
	lines := pollServiceLogLines(t, ctx, cp, "user-a", svc.ID, 2, 90*time.Second)
	var before, during int
	for _, line := range lines {
		switch line.GetLine() {
		case beforeMarker:
			before++
		case duringMarker:
			during++
		}
	}
	if before != 1 || during != 1 {
		t.Fatalf("after outage: before=%d during=%d, want 1 each", before, during)
	}
}

func TestDurableLogGapsAttributeToAllocationOwner(t *testing.T) {
	clickhouseURL := startTestClickHouse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		agentID = "scoping-agent"
		token   = "scoping-bootstrap"
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
	projectsA, err := store.catalog.listProjects(ctx, testUser("user-a"), false)
	if err != nil || len(projectsA) != 1 {
		t.Fatalf("projects A: %v", err)
	}
	projectsB, err := store.catalog.listProjects(ctx, testUser("user-b"), false)
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
	now := time.Now().UTC().Truncate(time.Second)

	// A drop summary without producer metadata (the restart case)
	// derives its service from the allocation owner and still lands
	// as an explicit gap.
	sendDurableBatch(t, stream, agentID, nil,
		&platformv1.LogDropSummary{
			AllocationId: allocA,
			LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
			Stream:       "stdout",
			DroppedCount: 9,
			Reason:       logpipeline.ReasonCorruptSpool,
			WindowStart:  timestamppb.New(now.Add(-time.Minute)),
			WindowEnd:    timestamppb.New(now),
		},
	)
	userCtx := userContext(t, cp, ctx, "user-a")
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := cp.dashboard.ListServiceLogs(userCtx, &platformv1.ListServiceLogsRequest{ServiceId: svcA.ID})
		if err != nil {
			return false, err
		}
		for _, gap := range resp.GetGaps() {
			if gap.GetDroppedCount() == 9 && gap.GetReason() == logpipeline.ReasonCorruptSpool && gap.GetAllocationId() == allocA {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("ownerless drop summary did not derive its service: %v", err)
	}
	respB, err := cp.dashboard.ListServiceLogs(userContext(t, cp, ctx, "user-b"), &platformv1.ListServiceLogsRequest{ServiceId: svcB.ID})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(respB.GetGaps()) != 0 {
		t.Fatalf("drop derivation leaked %d gaps to another tenant: %+v", len(respB.GetGaps()), respB.GetGaps())
	}

	// A drop summary claiming another tenant's service on this
	// agent's allocation is rejected, never written anywhere.
	spoof := &agentv1.LogBatch{
		AgentId: agentID,
		Drops: []*platformv1.LogDropSummary{{
			ServiceId:    svcB.ID,
			AllocationId: allocA,
			DroppedCount: 9,
			Reason:       logpipeline.ReasonSpoolOverflow,
			WindowStart:  timestamppb.New(now),
			WindowEnd:    timestamppb.New(now),
		}},
	}
	if err := store.fleet.validateAgentLogBatch(ctx, agentID, spoof); err == nil {
		t.Fatal("mismatched drop claim must reject the batch")
	}

	// A batch referencing an allocation this agent does not own is
	// rejected outright.
	foreign := &agentv1.LogBatch{
		AgentId: agentID,
		Drops: []*platformv1.LogDropSummary{{
			ServiceId:    svcA.ID,
			AllocationId: "alloc-never-assigned",
			DroppedCount: 3,
			Reason:       logpipeline.ReasonRateLimited,
			WindowStart:  timestamppb.New(now),
			WindowEnd:    timestamppb.New(now),
		}},
	}
	if err := store.fleet.validateAgentLogBatch(ctx, agentID, foreign); err == nil {
		t.Fatal("foreign allocation must reject the batch")
	}

	// Drop reports without an allocation cannot be attributed and
	// never become gap rows.
	unattributable := &agentv1.LogBatch{
		AgentId: agentID,
		Drops: []*platformv1.LogDropSummary{{
			ServiceId:    svcA.ID,
			DroppedCount: 4,
			Reason:       logpipeline.ReasonRateLimited,
			WindowStart:  timestamppb.New(now),
			WindowEnd:    timestamppb.New(now),
		}},
	}
	if err := store.fleet.validateAgentLogBatch(ctx, agentID, unattributable); err != nil {
		t.Fatalf("unattributable drops must scope away, not error: %v", err)
	}
	if len(unattributable.GetDrops()) != 0 {
		t.Fatalf("unattributable drops survived scoping: %+v", unattributable.GetDrops())
	}
	_ = svcB
}
