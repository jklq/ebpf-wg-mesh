package publicapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakePlatform struct {
	platformv1.UnimplementedPlatformServiceServer
	deletedProjects []string
	createdProjects []*platformv1.CreateProjectRequest
	lastUserID      string
	getServiceReqs  []*platformv1.GetServiceRequest
}

func (f *fakePlatform) noteUser(ctx context.Context) {
	if user, err := identity.DelegatedUserFromContext(ctx); err == nil {
		f.lastUserID = user.UserID
	}
}

func (f *fakePlatform) ListWorkspaces(ctx context.Context, _ *platformv1.ListWorkspacesRequest) (*platformv1.ListWorkspacesResponse, error) {
	f.noteUser(ctx)
	return &platformv1.ListWorkspacesResponse{Workspaces: []*platformv1.Workspace{
		{Id: "ws-1", Name: "Acme"}, {Id: "ws-2", Name: "Other"},
	}}, nil
}

func (f *fakePlatform) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	f.noteUser(ctx)
	f.createdProjects = append(f.createdProjects, req)
	return &platformv1.Project{Id: "proj-new", Name: req.GetName(), WorkspaceId: req.GetWorkspaceId()}, nil
}

func (f *fakePlatform) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	f.noteUser(ctx)
	if req.GetProjectId() == "missing" {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return &platformv1.Project{Id: req.GetProjectId(), Name: "demo"}, nil
}

func (f *fakePlatform) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	f.noteUser(ctx)
	f.getServiceReqs = append(f.getServiceReqs, req)
	return &platformv1.Service{Id: req.GetServiceId(), Name: "web"}, nil
}

func (f *fakePlatform) DeleteProject(_ context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
	if req.GetConfirmationName() != "demo" {
		return nil, status.Error(codes.InvalidArgument, "confirmation name mismatch")
	}
	f.deletedProjects = append(f.deletedProjects, req.GetProjectId())
	return &emptypb.Empty{}, nil
}

type fakeAuth struct{ tokens map[string]Token }

func (a fakeAuth) Authenticate(_ context.Context, secret string) (Token, error) {
	token, ok := a.tokens[secret]
	if !ok {
		return Token{}, ErrInvalidToken
	}
	return token, nil
}

func (a fakeAuth) TouchLastUsed(context.Context, string) {}

type fakeScopes struct{ byKindID map[string]ResourceScope }

func (f fakeScopes) Resolve(_ context.Context, kind, id string) (ResourceScope, bool, error) {
	scope, ok := f.byKindID[kind+":"+id]
	return scope, ok, nil
}

func testServer(t *testing.T, cfg Config) (*Server, *fakePlatform) {
	t.Helper()
	platform := &fakePlatform{}
	auth := fakeAuth{tokens: map[string]Token{
		"acct":   {ID: "tok-acct", UserID: "user-1", Email: "u@example.com", Scope: ScopeAccount, Access: AccessFull},
		"reader": {ID: "tok-read", UserID: "user-1", Scope: ScopeAccount, Access: AccessRead},
		"ws":     {ID: "tok-ws", UserID: "user-1", Scope: ScopeWorkspace, Access: AccessFull, WorkspaceID: "ws-1"},
		"proj": {ID: "tok-proj", UserID: "user-1", Scope: ScopeProject, Access: AccessFull,
			WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-1"},
	}}
	scopes := fakeScopes{byKindID: map[string]ResourceScope{
		"project:proj-1":    {WorkspaceID: "ws-1", ProjectID: "proj-1"},
		"project:proj-2":    {WorkspaceID: "ws-2", ProjectID: "proj-2"},
		"environment:env-1": {WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-1"},
		"environment:env-9": {WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-9"},
		"service:svc-1":     {WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-1", ServiceID: "svc-1"},
		"service:svc-9":     {WorkspaceID: "ws-1", ProjectID: "proj-1", EnvironmentID: "env-9", ServiceID: "svc-9"},
		"workspace:ws-1":    {WorkspaceID: "ws-1"},
		"workspace:ws-2":    {WorkspaceID: "ws-2"},
	}}
	return New(platform, auth, scopes, cfg), platform
}

func call(t *testing.T, server *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not the public shape: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

func TestAuthnAndDelegatedUser(t *testing.T) {
	server, platform := testServer(t, Config{})

	if rec := call(t, server, "GET", "/v1/projects/proj-1", "", ""); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing token: %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if rec := call(t, server, "GET", "/v1/projects/proj-1", "bogus", ""); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthenticated" {
		t.Fatalf("bad token: %d %s", rec.Code, rec.Body.String())
	}
	rec := call(t, server, "GET", "/v1/projects/proj-1", "acct", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("account token: %d %s", rec.Code, rec.Body.String())
	}
	if platform.lastUserID != "user-1" {
		t.Fatalf("platform saw user %q, want the token owner", platform.lastUserID)
	}
	if !strings.Contains(rec.Body.String(), `"name":"demo"`) {
		t.Fatalf("proto JSON body = %s", rec.Body.String())
	}
	if rec.Header().Get("X-RateLimit-Limit") == "" || rec.Header().Get("X-Request-Id") == "" {
		t.Fatalf("missing rate/request headers: %v", rec.Header())
	}
}

func TestErrorMapping(t *testing.T) {
	server, _ := testServer(t, Config{})
	rec := call(t, server, "GET", "/v1/projects/missing", "acct", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
		t.Fatalf("not found mapping: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, server, "GET", "/nope", "acct", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", rec.Code)
	}
}

func TestReadOnlyTokenCannotWrite(t *testing.T) {
	server, platform := testServer(t, Config{})
	rec := call(t, server, "POST", "/v1/projects", "reader", `{"name":"x"}`)
	if rec.Code != http.StatusForbidden || len(platform.createdProjects) != 0 {
		t.Fatalf("read token create: %d, created %d", rec.Code, len(platform.createdProjects))
	}
}

func TestReadOnlyTokenCannotRevealSecrets(t *testing.T) {
	server, platform := testServer(t, Config{})

	rec := call(t, server, "GET", "/v1/services/svc-1?include_secrets=true", "reader", "")
	if rec.Code != http.StatusForbidden || len(platform.getServiceReqs) != 0 {
		t.Fatalf("read token reveal: %d, platform calls %d", rec.Code, len(platform.getServiceReqs))
	}
	// The same token still gets the redacted read.
	if rec := call(t, server, "GET", "/v1/services/svc-1", "reader", ""); rec.Code != http.StatusOK {
		t.Fatalf("read token get: %d", rec.Code)
	}
	if platform.getServiceReqs[0].GetIncludeSecrets() {
		t.Fatal("default get forwarded include_secrets")
	}
	// A full-access token may ask, and the flag reaches the platform.
	if rec := call(t, server, "GET", "/v1/services/svc-1?include_secrets=true", "acct", ""); rec.Code != http.StatusOK {
		t.Fatalf("full token reveal: %d", rec.Code)
	}
	if !platform.getServiceReqs[1].GetIncludeSecrets() {
		t.Fatal("full token get did not forward include_secrets")
	}
}

func TestWorkspaceScopeConfinement(t *testing.T) {
	server, _ := testServer(t, Config{})

	if rec := call(t, server, "GET", "/v1/projects/proj-1", "ws", ""); rec.Code != http.StatusOK {
		t.Fatalf("own workspace project: %d", rec.Code)
	}
	if rec := call(t, server, "GET", "/v1/projects/proj-2", "ws", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("other workspace project: %d, want 403", rec.Code)
	}
	// Unknown resources pass the scope gate; the platform answers.
	if rec := call(t, server, "GET", "/v1/projects/missing", "ws", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project: %d, want platform 404", rec.Code)
	}
	// Workspace token sees only its workspace in the list.
	rec := call(t, server, "GET", "/v1/workspaces", "ws", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ws-2") {
		t.Fatalf("filtered workspaces: %d %s", rec.Code, rec.Body.String())
	}
	// /me is account-only; /token works for every scope.
	if rec := call(t, server, "GET", "/v1/me", "ws", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("workspace token /me: %d, want 403", rec.Code)
	}
	if rec := call(t, server, "GET", "/v1/token", "ws", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scope":"workspace"`) {
		t.Fatalf("/token: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProjectTokenTiersAndConfinement(t *testing.T) {
	server, _ := testServer(t, Config{})

	// Project tokens cannot list projects (workspace tier).
	if rec := call(t, server, "GET", "/v1/projects", "proj", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("project token list projects: %d, want 403", rec.Code)
	}
	// Own environment's service is fine (fake GetService is unimplemented,
	// so a non-403 response proves the gate passed); other env is denied.
	if rec := call(t, server, "GET", "/v1/services/svc-9/status", "proj", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("other environment service: %d, want 403", rec.Code)
	}
	if rec := call(t, server, "GET", "/v1/services/svc-1/status", "proj", ""); rec.Code == http.StatusForbidden {
		t.Fatalf("own environment service denied: %d", rec.Code)
	}
	// Delete project is workspace-tier: denied even for its own project.
	if rec := call(t, server, "DELETE", "/v1/projects/proj-1", "proj", `{"confirmation_name":"demo"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("project token delete project: %d, want 403", rec.Code)
	}
}

func TestCreateAndDeleteFlow(t *testing.T) {
	server, platform := testServer(t, Config{})

	rec := call(t, server, "POST", "/v1/projects", "ws", `{"name":"new"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := platform.createdProjects[0].GetWorkspaceId(); got != "ws-1" {
		t.Fatalf("workspace token create forced workspace = %q, want its own", got)
	}
	rec = call(t, server, "DELETE", "/v1/projects/proj-1", "ws", `{"confirmation_name":"demo"}`)
	if rec.Code != http.StatusNoContent || len(platform.deletedProjects) != 1 {
		t.Fatalf("delete: %d deleted=%v", rec.Code, platform.deletedProjects)
	}
	rec = call(t, server, "POST", "/v1/projects", "ws", `{"name":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	server, _ := testServer(t, Config{RequestsPerHour: 3, BurstPerSecond: 100})
	for i := range 3 {
		if rec := call(t, server, "GET", "/v1/projects/proj-1", "acct", ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := call(t, server, "GET", "/v1/projects/proj-1", "acct", "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || errorCode(t, rec) != "rate_limited" {
		t.Fatalf("over limit: %d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	// A different token has its own budget.
	if rec := call(t, server, "GET", "/v1/projects/proj-1", "reader", ""); rec.Code != http.StatusOK {
		t.Fatalf("other token blocked too: %d", rec.Code)
	}
}

func TestOpenAPIDocument(t *testing.T) {
	server, _ := testServer(t, Config{})
	rec := call(t, server, "GET", "/v1/openapi.json", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi: %d %s", rec.Code, rec.Body.String()[:200])
	}
	var doc struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("openapi is not JSON: %v", err)
	}
	if len(doc.Paths) < 40 {
		t.Fatalf("paths = %d, want the full route table", len(doc.Paths))
	}
	for _, want := range []string{"/v1/projects/{projectId}", "/v1/services/{serviceId}/logs", "/v1/environments/{environmentId}/releases"} {
		if _, ok := doc.Paths[want]; !ok {
			t.Fatalf("missing path %s", want)
		}
	}
	for _, want := range []string{"Project", "Service", "ServiceSpec", "Error"} {
		if _, ok := doc.Components.Schemas[want]; !ok {
			t.Fatalf("missing schema %s", want)
		}
	}
}
