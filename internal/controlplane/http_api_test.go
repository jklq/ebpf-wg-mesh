package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestHTTPAPIRejectsUnauthenticatedRequests(t *testing.T) {
	handler := NewHTTPAPIHandler(
		httpValidatorFunc(func(r *http.Request) (Identity, error) {
			return Identity{}, status.Error(codes.Unauthenticated, "missing bearer token")
		}),
		&fakeHTTPPlatform{},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"code":"Unauthenticated"`) {
		t.Fatalf("unexpected body %s", body)
	}
}

func TestHTTPAPIListProjectsReturnsJSON(t *testing.T) {
	handler := NewHTTPAPIHandler(
		httpValidatorFunc(func(r *http.Request) (Identity, error) {
			return Identity{Subject: "user-1", Email: "user@example.com"}, nil
		}),
		&fakeHTTPPlatform{
			listProjects: func(ctx context.Context, req *emptypb.Empty) (*platformv1.ListProjectsResponse, error) {
				if identity, err := IdentityFromContext(ctx); err != nil {
					t.Fatalf("IdentityFromContext: %v", err)
				} else if identity.Subject != "user-1" {
					t.Fatalf("unexpected identity %+v", identity)
				}
				return &platformv1.ListProjectsResponse{
					Projects: []*platformv1.Project{{
						Id:   "project-1",
						Name: "demo",
					}},
				}, nil
			},
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("unexpected content type %q", contentType)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"projects":[`) || !strings.Contains(body, `"name":"demo"`) {
		t.Fatalf("unexpected body %s", body)
	}
}

func TestHTTPAPICreateProjectRejectsInvalidJSON(t *testing.T) {
	handler := NewHTTPAPIHandler(
		httpValidatorFunc(func(r *http.Request) (Identity, error) {
			return Identity{Subject: "user-1", Email: "user@example.com"}, nil
		}),
		&fakeHTTPPlatform{},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader("{"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"code":"InvalidArgument"`) {
		t.Fatalf("unexpected body %s", body)
	}
}

func TestHTTPStatusForCode(t *testing.T) {
	if got := httpStatusForCode(codes.PermissionDenied); got != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", got)
	}
	if got := httpStatusForCode(codes.Unavailable); got != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", got)
	}
	if got := httpStatusForCode(codes.DataLoss); got != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", got)
	}
}

type httpValidatorFunc func(r *http.Request) (Identity, error)

func (f httpValidatorFunc) identityFromHTTPRequest(r *http.Request) (Identity, error) {
	return f(r)
}

type fakeHTTPPlatform struct {
	listProjects func(ctx context.Context, req *emptypb.Empty) (*platformv1.ListProjectsResponse, error)
}

func (f *fakeHTTPPlatform) ListProjects(ctx context.Context, req *emptypb.Empty) (*platformv1.ListProjectsResponse, error) {
	if f.listProjects != nil {
		return f.listProjects(ctx, req)
	}
	return &platformv1.ListProjectsResponse{}, nil
}

func (f *fakeHTTPPlatform) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) UpsertDomain(ctx context.Context, req *platformv1.UpsertDomainRequest) (*platformv1.Service, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) DeleteDomain(ctx context.Context, req *platformv1.DeleteDomainRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (f *fakeHTTPPlatform) ListAgents(ctx context.Context, req *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}
