package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type httpAPI struct {
	validator *Validator
	platform  *PlatformService
	marshal   protojson.MarshalOptions
	unmarshal protojson.UnmarshalOptions
}

func NewHTTPAPIHandler(validator *Validator, platform *PlatformService) http.Handler {
	api := &httpAPI{
		validator: validator,
		platform:  platform,
		marshal: protojson.MarshalOptions{
			UseProtoNames: true,
		},
		unmarshal: protojson.UnmarshalOptions{
			DiscardUnknown: false,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	api.handle(mux, "GET /v1/projects", func(ctx context.Context, _ *http.Request) (proto.Message, error) {
		return api.platform.ListProjects(ctx, &emptypb.Empty{})
	})
	api.handle(mux, "POST /v1/projects", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		req := &platformv1.CreateProjectRequest{}
		if err := api.decodeBody(r, req); err != nil {
			return nil, err
		}
		return api.platform.CreateProject(ctx, req)
	})
	api.handle(mux, "GET /v1/projects/{project_id}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.GetProject(ctx, &platformv1.GetProjectRequest{ProjectId: r.PathValue("project_id")})
	})
	api.handle(mux, "GET /v1/projects/{project_id}/services", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.ListServices(ctx, &platformv1.ListServicesRequest{ProjectId: r.PathValue("project_id")})
	})
	api.handle(mux, "POST /v1/projects/{project_id}/services", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		req := &platformv1.CreateServiceRequest{ProjectId: r.PathValue("project_id")}
		if err := api.decodeBody(r, req); err != nil {
			return nil, err
		}
		req.ProjectId = r.PathValue("project_id")
		return api.platform.CreateService(ctx, req)
	})
	api.handle(mux, "GET /v1/projects/{project_id}/services/{service_id}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.GetService(ctx, &platformv1.GetServiceRequest{
			ProjectId: r.PathValue("project_id"),
			ServiceId: r.PathValue("service_id"),
		})
	})
	api.handle(mux, "PUT /v1/projects/{project_id}/services/{service_id}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		req := &platformv1.UpdateServiceRequest{
			ProjectId: r.PathValue("project_id"),
			ServiceId: r.PathValue("service_id"),
		}
		if err := api.decodeBody(r, req); err != nil {
			return nil, err
		}
		req.ProjectId = r.PathValue("project_id")
		req.ServiceId = r.PathValue("service_id")
		return api.platform.UpdateService(ctx, req)
	})
	api.handle(mux, "DELETE /v1/projects/{project_id}/services/{service_id}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.DeleteService(ctx, &platformv1.DeleteServiceRequest{
			ProjectId: r.PathValue("project_id"),
			ServiceId: r.PathValue("service_id"),
		})
	})
	api.handle(mux, "GET /v1/projects/{project_id}/services/{service_id}/status", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.GetServiceStatus(ctx, &platformv1.GetServiceStatusRequest{
			ProjectId: r.PathValue("project_id"),
			ServiceId: r.PathValue("service_id"),
		})
	})
	api.handle(mux, "PUT /v1/projects/{project_id}/services/{service_id}/domains/{domain}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.UpsertDomain(ctx, &platformv1.UpsertDomainRequest{
			ProjectId: r.PathValue("project_id"),
			ServiceId: r.PathValue("service_id"),
			Domain:    r.PathValue("domain"),
		})
	})
	api.handle(mux, "GET /v1/projects/{project_id}/volumes", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.ListVolumes(ctx, &platformv1.ListVolumesRequest{ProjectId: r.PathValue("project_id")})
	})
	api.handle(mux, "POST /v1/projects/{project_id}/volumes", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		req := &platformv1.CreateVolumeRequest{ProjectId: r.PathValue("project_id")}
		if err := api.decodeBody(r, req); err != nil {
			return nil, err
		}
		req.ProjectId = r.PathValue("project_id")
		return api.platform.CreateVolume(ctx, req)
	})
	api.handle(mux, "DELETE /v1/projects/{project_id}/volumes/{volume_id}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.DeleteVolume(ctx, &platformv1.DeleteVolumeRequest{
			ProjectId: r.PathValue("project_id"),
			VolumeId:  r.PathValue("volume_id"),
		})
	})
	api.handle(mux, "DELETE /v1/projects/{project_id}/domains/{domain}", func(ctx context.Context, r *http.Request) (proto.Message, error) {
		return api.platform.DeleteDomain(ctx, &platformv1.DeleteDomainRequest{
			ProjectId: r.PathValue("project_id"),
			Domain:    r.PathValue("domain"),
		})
	})
	api.handle(mux, "GET /v1/agents", func(ctx context.Context, _ *http.Request) (proto.Message, error) {
		return api.platform.ListAgents(ctx, &emptypb.Empty{})
	})

	return mux
}

func (a *httpAPI) handle(mux *http.ServeMux, pattern string, fn func(context.Context, *http.Request) (proto.Message, error)) {
	mux.Handle(pattern, a.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := fn(r.Context(), r)
		if err != nil {
			a.writeError(w, err)
			return
		}
		a.writeProto(w, http.StatusOK, resp)
	})))
}

func (a *httpAPI) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := a.validator.identityFromHTTPRequest(r)
		if err != nil {
			a.writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	})
}

func (a *httpAPI) decodeBody(r *http.Request, msg proto.Message) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "read request body: %v", err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := a.unmarshal.Unmarshal(body, msg); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid json body: %v", err)
	}
	return nil
}

func (a *httpAPI) writeProto(w http.ResponseWriter, statusCode int, msg proto.Message) {
	if msg == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		return
	}
	body, err := a.marshal.Marshal(msg)
	if err != nil {
		a.writeError(w, status.Errorf(codes.Internal, "marshal response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func (a *httpAPI) writeError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.New(codes.Internal, err.Error())
	}
	code := st.Code()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatusForCode(code))
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    code.String(),
		"message": st.Message(),
	})
}

func httpStatusForCode(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Aborted, codes.AlreadyExists:
		return http.StatusConflict
	case codes.FailedPrecondition:
		return http.StatusPreconditionFailed
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}
