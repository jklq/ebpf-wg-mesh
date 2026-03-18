package controlplane

import (
	"context"
	"encoding/json"
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
)

type httpIdentityValidator interface {
	identityFromHTTPRequest(r *http.Request) (Identity, error)
}

type httpPlatform interface {
	platformv1.PlatformServiceServer
}

type authenticatedPlatformService struct {
	platformv1.UnimplementedPlatformServiceServer
	next httpPlatform
}

func NewHTTPAPIHandler(validator httpIdentityValidator, platform httpPlatform) http.Handler {
	gateway := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{UseProtoNames: true},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: false,
			},
		}),
		runtime.WithErrorHandler(writeGatewayError),
	)
	_ = platformv1.RegisterPlatformServiceHandlerServer(context.Background(), gateway, &authenticatedPlatformService{next: platform})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	root.Handle("/", authenticateHTTP(gateway, validator))
	return root
}

func authenticateHTTP(next http.Handler, validator httpIdentityValidator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := validator.identityFromHTTPRequest(r)
		if err != nil {
			writeGatewayError(r.Context(), nil, &runtime.JSONPb{}, w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), delegatedUserContextKey{}, DelegatedUser{
			Subject: identity.Subject,
			Email:   identity.Email,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *authenticatedPlatformService) authorizedContext(ctx context.Context) (context.Context, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (s *authenticatedPlatformService) EnsurePrincipal(ctx context.Context, req *platformv1.EnsurePrincipalRequest) (*platformv1.Principal, error) {
	return s.next.EnsurePrincipal(ctx, req)
}

func (s *authenticatedPlatformService) CreateProject(ctx context.Context, req *platformv1.CreateProjectRequest) (*platformv1.Project, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.CreateProject(ctx, req)
}

func (s *authenticatedPlatformService) ListProjects(ctx context.Context, req *emptypb.Empty) (*platformv1.ListProjectsResponse, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.ListProjects(ctx, req)
}

func (s *authenticatedPlatformService) GetProject(ctx context.Context, req *platformv1.GetProjectRequest) (*platformv1.Project, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.GetProject(ctx, req)
}

func (s *authenticatedPlatformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.CreateService(ctx, req)
}

func (s *authenticatedPlatformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.UpdateService(ctx, req)
}

func (s *authenticatedPlatformService) RedeployService(ctx context.Context, req *platformv1.RedeployServiceRequest) (*platformv1.ServiceStatus, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.RedeployService(ctx, req)
}

func (s *authenticatedPlatformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.DeleteService(ctx, req)
}

func (s *authenticatedPlatformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.GetService(ctx, req)
}

func (s *authenticatedPlatformService) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.ListServices(ctx, req)
}

func (s *authenticatedPlatformService) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.CreateVolume(ctx, req)
}

func (s *authenticatedPlatformService) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.DeleteVolume(ctx, req)
}

func (s *authenticatedPlatformService) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.ListVolumes(ctx, req)
}

func (s *authenticatedPlatformService) CreateDomainBinding(ctx context.Context, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.CreateDomainBinding(ctx, req)
}

func (s *authenticatedPlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.GetDomainBinding(ctx, req)
}

func (s *authenticatedPlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.ListDomainBindings(ctx, req)
}

func (s *authenticatedPlatformService) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.UpdateDomainBinding(ctx, req)
}

func (s *authenticatedPlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.DeleteDomainBinding(ctx, req)
}

func (s *authenticatedPlatformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.GetServiceStatus(ctx, req)
}

func (s *authenticatedPlatformService) ListAgents(ctx context.Context, req *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
	ctx, err := s.authorizedContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.next.ListAgents(ctx, req)
}

func writeGatewayError(_ context.Context, _ *runtime.ServeMux, marshaler runtime.Marshaler, w http.ResponseWriter, _ *http.Request, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.New(codes.Internal, err.Error())
	}
	w.Header().Set("Content-Type", marshaler.ContentType(nil))
	w.WriteHeader(httpStatusForCode(st.Code()))
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    st.Code().String(),
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
