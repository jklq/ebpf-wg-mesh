package apiclient

import (
	"context"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsAuthAndDecodesProto(t *testing.T) {
	t.Parallel()
	var sawAuth, sawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawPath = r.Header.Get("Authorization"), r.URL.Path+"?"+r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"projects":[{"id":"p1","name":"demo"}]}`)
	}))
	defer server.Close()

	client := New(server.URL, "wgm_test")
	resp, err := client.ListProjects(context.Background(), &platformv1.ListProjectsRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer wgm_test" {
		t.Fatalf("authorization = %q", sawAuth)
	}
	if !strings.Contains(sawPath, "/v1/projects") || !strings.Contains(sawPath, "workspace_id=ws-1") {
		t.Fatalf("path = %q", sawPath)
	}
	if len(resp.GetProjects()) != 1 || resp.GetProjects()[0].GetName() != "demo" {
		t.Fatalf("resp = %+v", resp.GetProjects())
	}
}

func TestClientMapsAPIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-9")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"not_found","message":"project not found","request_id":"req-9"}}`)
	}))
	defer server.Close()

	_, err := New(server.URL, "wgm_test").GetProject(context.Background(), "missing")
	if !IsCode(err, "not_found") {
		t.Fatalf("err = %v, want not_found code", err)
	}
	if !strings.Contains(fmt.Sprint(err), "req-9") {
		t.Fatalf("err lacks request id: %v", err)
	}
}

func TestClientRetriesIdempotentGET(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"id":"tok-1","scope":"account","access":"full","user_id":"u1"}`)
	}))
	defer server.Close()

	info, err := New(server.URL, "wgm_test").Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || info.UserID != "u1" {
		t.Fatalf("calls=%d info=%+v", calls, info)
	}
}
