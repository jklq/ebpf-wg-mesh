package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/apiclient"
	"ebof-wg-mesh/internal/publicapi"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakePlatform is a small stateful PlatformService for end-to-end tests:
// MCP client -> mcpserver -> apiclient -> publicapi -> fake.
type fakePlatform struct {
	platformv1.UnimplementedPlatformServiceServer
	mu       sync.Mutex
	service  *platformv1.Service
	deleted  []string
	released []string
	created  []*platformv1.CreateServiceRequest
}

func newFakePlatform() *fakePlatform {
	replicas := int32(1)
	return &fakePlatform{service: &platformv1.Service{
		Id: "svc-1", EnvironmentId: "env-1", Name: "web",
		SpecRevision: 3, PendingChanges: true, DesiredReplicaCount: 1,
		Spec: &platformv1.ServiceSpec{
			Runtime: &platformv1.ServiceRuntime{
				Env:   map[string]string{"EXISTING": "1"},
				Ports: []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
			},
			Source:              &platformv1.ServiceSource{Source: &platformv1.ServiceSource_Image{Image: &platformv1.DirectImageSource{Image: "nginx:1.27"}}},
			DesiredReplicaCount: &replicas,
		},
		UnappliedChanges: []*platformv1.ServiceUnappliedChange{{Id: "ch-1", Field: "env.NEW", NewValue: "2"}},
	}}
}

func (f *fakePlatform) GetProject(_ context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	if req.GetProjectId() != "proj-1" {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return &platformv1.Project{Id: "proj-1", Name: "demo", WorkspaceId: "ws-1"}, nil
}

func (f *fakePlatform) ListProjects(context.Context, *platformv1.ListProjectsRequest) (*platformv1.ListProjectsResponse, error) {
	return &platformv1.ListProjectsResponse{Projects: []*platformv1.Project{{Id: "proj-1", Name: "demo", WorkspaceId: "ws-1"}}}, nil
}

func (f *fakePlatform) PreviewProjectDeletion(context.Context, *platformv1.PreviewProjectDeletionRequest) (*platformv1.DeletionPreview, error) {
	return &platformv1.DeletionPreview{Services: []*platformv1.DeletionPreviewService{{Id: "svc-1", Name: "web"}}}, nil
}

func (f *fakePlatform) DeleteProject(_ context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, req.GetProjectId()+":"+req.GetConfirmationName())
	return &emptypb.Empty{}, nil
}

func (f *fakePlatform) GetService(_ context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.GetServiceId() != f.service.GetId() {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	return proto.Clone(f.service).(*platformv1.Service), nil
}

func (f *fakePlatform) UpdateService(_ context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.GetExpectedSpecRevision() != f.service.GetSpecRevision() {
		return nil, status.Error(codes.Aborted, "stale draft")
	}
	f.service.Spec = req.GetService().GetSpec()
	f.service.SpecRevision++
	return proto.Clone(f.service).(*platformv1.Service), nil
}

func (f *fakePlatform) CreateService(_ context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return &platformv1.Service{Id: "svc-new", EnvironmentId: req.GetEnvironmentId(), Name: req.GetService().GetName(), Spec: req.GetService().GetSpec()}, nil
}

func (f *fakePlatform) GetServiceStatus(_ context.Context, _ *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &platformv1.ServiceStatus{Service: proto.Clone(f.service).(*platformv1.Service)}, nil
}

func (f *fakePlatform) ListServices(_ context.Context, _ *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &platformv1.ListServicesResponse{Services: []*platformv1.Service{proto.Clone(f.service).(*platformv1.Service)}}, nil
}

func (f *fakePlatform) ReleaseEnvironment(_ context.Context, req *platformv1.ReleaseEnvironmentRequest) (*platformv1.ReleaseEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, req.GetEnvironmentId())
	f.service.PendingChanges = false
	return &platformv1.ReleaseEnvironmentResponse{Services: []*platformv1.ServiceStatus{{Service: proto.Clone(f.service).(*platformv1.Service)}}}, nil
}

func (f *fakePlatform) ListServiceDeployments(context.Context, *platformv1.ListServiceDeploymentsRequest) (*platformv1.ListServiceDeploymentsResponse, error) {
	return &platformv1.ListServiceDeploymentsResponse{Deployments: []*platformv1.DeploymentRecord{
		{Id: "dep-1", ServiceId: "svc-1", Status: &platformv1.DeploymentStatus{State: platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED, ReasonCode: "EXIT_NONZERO", Detail: "exit 1"}},
	}}, nil
}

func (f *fakePlatform) ListServiceLogs(_ context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	return &platformv1.ListServiceLogsResponse{Lines: []*platformv1.ServiceLogLine{
		{Line: "listening on 8080", LogType: req.GetLogType()}, {Line: "fatal: missing config", LogType: req.GetLogType()},
	}}, nil
}

func (f *fakePlatform) ListDomainBindings(context.Context, *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	return &platformv1.ListDomainBindingsResponse{}, nil
}

func (f *fakePlatform) GetEnvironment(_ context.Context, req *platformv1.GetEnvironmentRequest) (*platformv1.Environment, error) {
	return &platformv1.Environment{Id: req.GetEnvironmentId(), ProjectId: "proj-1", Name: "production", IsProduction: true}, nil
}

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, secret string) (publicapi.Token, error) {
	if secret != "wgm_test" {
		return publicapi.Token{}, publicapi.ErrInvalidToken
	}
	return publicapi.Token{ID: "tok-1", UserID: "user-1", Email: "u@example.com", Scope: publicapi.ScopeAccount, Access: publicapi.AccessFull}, nil
}

func (fakeAuth) TouchLastUsed(context.Context, string) {}

type fakeScopes struct{}

func (fakeScopes) Resolve(_ context.Context, kind, id string) (publicapi.ResourceScope, bool, error) {
	return publicapi.ResourceScope{WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-1"}, true, nil
}

func newSession(t *testing.T) (*mcp.ClientSession, *fakePlatform) {
	t.Helper()
	platform := newFakePlatform()
	api := publicapi.New(platform, fakeAuth{}, fakeScopes{}, publicapi.Config{})
	httpServer := httptest.NewServer(api)
	t.Cleanup(httpServer.Close)

	server, err := New(apiclient.New(httpServer.URL, "wgm_test"), Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, platform
}

func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("CallTool %s returned tool error: %s", name, textOf(result))
	}
	return textOf(result)
}

func textOf(result *mcp.CallToolResult) string {
	var out strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	return out.String()
}

func TestToolSurfaceAndAnnotations(t *testing.T) {
	session, _ := newSession(t)
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	for _, want := range []string{
		"whoami", "list_projects", "create_project", "list_services", "create_service",
		"update_service", "scale_service", "remove_service", "list_variables", "set_variables",
		"list_deployments", "redeploy", "restart_service", "release_environment", "deploy",
		"generate_domain", "list_domains", "create_volume", "get_logs", "get_build_logs",
		"service_metrics", "get_project_usage", "diagnose_service", "docs_search", "docs_fetch",
		"get_billing_account", "environment_status", "inspect_source",
	} {
		if byName[want] == nil {
			t.Fatalf("missing tool %s (have %d)", want, len(byName))
		}
	}
	if len(byName) < 60 {
		t.Fatalf("tool count = %d, want full parity surface", len(byName))
	}
	if del := byName["delete_project"]; del.Annotations == nil || del.Annotations.DestructiveHint == nil || !*del.Annotations.DestructiveHint {
		t.Fatalf("delete_project must be destructiveHint: %+v", del.Annotations)
	}
	if who := byName["whoami"]; who.Annotations == nil || !who.Annotations.ReadOnlyHint {
		t.Fatalf("whoami must be readOnlyHint")
	}
}

func TestDestructivePreviewThenConfirm(t *testing.T) {
	session, platform := newSession(t)

	preview := callText(t, session, "delete_project", map[string]any{"project_id": "proj-1"})
	if !strings.Contains(preview, `"executed":false`) || !strings.Contains(preview, `"requires_confirm":true`) {
		t.Fatalf("preview = %s", preview)
	}
	if len(platform.deleted) != 0 {
		t.Fatalf("preview executed a delete: %v", platform.deleted)
	}
	callText(t, session, "delete_project", map[string]any{"project_id": "proj-1", "confirm": true})
	if len(platform.deleted) != 1 || platform.deleted[0] != "proj-1:demo" {
		t.Fatalf("deleted = %v, want confirmation name resolved server-side", platform.deleted)
	}
}

func TestSetVariablesMergesAndReleases(t *testing.T) {
	session, platform := newSession(t)
	out := callText(t, session, "set_variables", map[string]any{
		"service_id": "svc-1",
		"set":        map[string]any{"NEW": "2"},
		"release":    true,
	})
	env := platform.service.GetSpec().GetRuntime().GetEnv()
	if env["EXISTING"] != "1" || env["NEW"] != "2" {
		t.Fatalf("merged env = %v", env)
	}
	if len(platform.released) != 1 || !strings.Contains(out, "released") {
		t.Fatalf("released = %v, out = %s", platform.released, out)
	}
}

func TestCreateServiceBuildsSpec(t *testing.T) {
	session, platform := newSession(t)
	callText(t, session, "create_service", map[string]any{
		"environment_id": "env-1", "name": "db", "image": "postgres:16",
		"ports": []any{5432},
	})
	if len(platform.created) != 1 {
		t.Fatalf("created = %d", len(platform.created))
	}
	spec := platform.created[0].GetService().GetSpec()
	if spec.GetSource().GetImage().GetImage() != "postgres:16" {
		t.Fatalf("source = %v", spec.GetSource())
	}
	if spec.GetRuntime().GetCpuMillis() != 250 || spec.GetRuntime().GetPorts()[0].GetPort() != 5432 {
		t.Fatalf("runtime defaults = %+v", spec.GetRuntime())
	}
}

func TestDiagnoseAndDocs(t *testing.T) {
	session, _ := newSession(t)
	diagnosis := callText(t, session, "diagnose_service", map[string]any{"service_id": "svc-1"})
	if !strings.Contains(diagnosis, "likely_issues") || !strings.Contains(diagnosis, "pending_changes") {
		t.Fatalf("diagnosis lacks issue synthesis: %s", diagnosis[:400])
	}
	if !strings.Contains(diagnosis, "fatal: missing config") {
		t.Fatalf("diagnosis lacks logs")
	}
	hits := callText(t, session, "docs_search", map[string]any{"query": "billing statement"})
	if !strings.Contains(hits, "billing") {
		t.Fatalf("docs_search = %s", hits)
	}
	resources, err := session.ListResources(context.Background(), &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range resources.Resources {
		if strings.HasSuffix(resource.URI, "billing.md") {
			found = true
		}
	}
	if !found {
		t.Fatalf("docs resources missing billing.md: %+v", resources.Resources)
	}
}

func TestHTTPTransportAuth(t *testing.T) {
	platform := newFakePlatform()
	apiServer := httptest.NewServer(publicapi.New(platform, fakeAuth{}, fakeScopes{}, publicapi.Config{}))
	defer apiServer.Close()
	handler, err := NewHTTPHandler(HTTPOptions{APIURL: apiServer.URL, PublicURL: "http://mcp.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	mcpServer := httptest.NewServer(handler)
	defer mcpServer.Close()

	resp, err := http.Post(mcpServer.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP request: %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("WWW-Authenticate = %q, want resource_metadata pointer", resp.Header.Get("WWW-Authenticate"))
	}
	prm, err := http.Get(mcpServer.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer prm.Body.Close()
	if prm.StatusCode != http.StatusOK {
		t.Fatalf("protected resource metadata: %d", prm.StatusCode)
	}
	req, _ := http.NewRequest("POST", mcpServer.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("Content-Type", "application/json")
	bad, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token MCP request: %d, want 401", bad.StatusCode)
	}
}
