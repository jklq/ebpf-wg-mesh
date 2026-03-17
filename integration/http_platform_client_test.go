package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type httpPlatformClient struct {
	baseURL   string
	host      string
	client    *http.Client
	marshal   protojson.MarshalOptions
	unmarshal protojson.UnmarshalOptions
}

func newHTTPPlatformClient(baseURL, host string) platformv1.PlatformServiceClient {
	return &httpPlatformClient{
		baseURL: baseURL,
		host:    host,
		client:  &http.Client{},
		marshal: protojson.MarshalOptions{
			UseProtoNames: true,
		},
		unmarshal: protojson.UnmarshalOptions{
			DiscardUnknown: false,
		},
	}
}

func (c *httpPlatformClient) CreateProject(ctx context.Context, in *platformv1.CreateProjectRequest, _ ...grpc.CallOption) (*platformv1.Project, error) {
	resp := &platformv1.Project{}
	return resp, c.do(ctx, http.MethodPost, "/v1/projects", in, resp)
}

func (c *httpPlatformClient) ListProjects(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*platformv1.ListProjectsResponse, error) {
	resp := &platformv1.ListProjectsResponse{}
	return resp, c.do(ctx, http.MethodGet, "/v1/projects", nil, resp)
}

func (c *httpPlatformClient) GetProject(ctx context.Context, in *platformv1.GetProjectRequest, _ ...grpc.CallOption) (*platformv1.Project, error) {
	resp := &platformv1.Project{}
	return resp, c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(in.GetProjectId()), nil, resp)
}

func (c *httpPlatformClient) CreateService(ctx context.Context, in *platformv1.CreateServiceRequest, _ ...grpc.CallOption) (*platformv1.Service, error) {
	resp := &platformv1.Service{}
	return resp, c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/services", in, resp)
}

func (c *httpPlatformClient) UpdateService(ctx context.Context, in *platformv1.UpdateServiceRequest, _ ...grpc.CallOption) (*platformv1.Service, error) {
	resp := &platformv1.Service{}
	return resp, c.do(ctx, http.MethodPut, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/services/"+url.PathEscape(in.GetServiceId()), in, resp)
}

func (c *httpPlatformClient) DeleteService(ctx context.Context, in *platformv1.DeleteServiceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	resp := &emptypb.Empty{}
	return resp, c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/services/"+url.PathEscape(in.GetServiceId()), nil, resp)
}

func (c *httpPlatformClient) GetService(ctx context.Context, in *platformv1.GetServiceRequest, _ ...grpc.CallOption) (*platformv1.Service, error) {
	resp := &platformv1.Service{}
	return resp, c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/services/"+url.PathEscape(in.GetServiceId()), nil, resp)
}

func (c *httpPlatformClient) ListServices(ctx context.Context, in *platformv1.ListServicesRequest, _ ...grpc.CallOption) (*platformv1.ListServicesResponse, error) {
	resp := &platformv1.ListServicesResponse{}
	return resp, c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/services", nil, resp)
}

func (c *httpPlatformClient) CreateVolume(ctx context.Context, in *platformv1.CreateVolumeRequest, _ ...grpc.CallOption) (*platformv1.Volume, error) {
	resp := &platformv1.Volume{}
	return resp, c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/volumes", in, resp)
}

func (c *httpPlatformClient) DeleteVolume(ctx context.Context, in *platformv1.DeleteVolumeRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	resp := &emptypb.Empty{}
	return resp, c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/volumes/"+url.PathEscape(in.GetVolumeId()), nil, resp)
}

func (c *httpPlatformClient) ListVolumes(ctx context.Context, in *platformv1.ListVolumesRequest, _ ...grpc.CallOption) (*platformv1.ListVolumesResponse, error) {
	resp := &platformv1.ListVolumesResponse{}
	return resp, c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(in.GetProjectId())+"/volumes", nil, resp)
}

func (c *httpPlatformClient) UpsertDomain(ctx context.Context, in *platformv1.UpsertDomainRequest, _ ...grpc.CallOption) (*platformv1.Service, error) {
	resp := &platformv1.Service{}
	path := "/v1/projects/" + url.PathEscape(in.GetProjectId()) + "/services/" + url.PathEscape(in.GetServiceId()) + "/domains/" + url.PathEscape(in.GetDomain())
	return resp, c.do(ctx, http.MethodPut, path, nil, resp)
}

func (c *httpPlatformClient) DeleteDomain(ctx context.Context, in *platformv1.DeleteDomainRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	resp := &emptypb.Empty{}
	path := "/v1/projects/" + url.PathEscape(in.GetProjectId()) + "/domains/" + url.PathEscape(in.GetDomain())
	return resp, c.do(ctx, http.MethodDelete, path, nil, resp)
}

func (c *httpPlatformClient) GetServiceStatus(ctx context.Context, in *platformv1.GetServiceStatusRequest, _ ...grpc.CallOption) (*platformv1.ServiceStatus, error) {
	resp := &platformv1.ServiceStatus{}
	path := "/v1/projects/" + url.PathEscape(in.GetProjectId()) + "/services/" + url.PathEscape(in.GetServiceId()) + "/status"
	return resp, c.do(ctx, http.MethodGet, path, nil, resp)
}

func (c *httpPlatformClient) ListAgents(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*platformv1.ListAgentsResponse, error) {
	resp := &platformv1.ListAgentsResponse{}
	return resp, c.do(ctx, http.MethodGet, "/v1/agents", nil, resp)
}

func (c *httpPlatformClient) do(ctx context.Context, method, path string, reqMsg proto.Message, respMsg proto.Message) error {
	var body io.Reader
	if reqMsg != nil {
		data, err := c.marshal.Marshal(reqMsg)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Host = c.host
	req.Header.Set("Accept", "application/json")
	if reqMsg != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if auth := md.Get("authorization"); len(auth) > 0 {
			req.Header.Set("Authorization", auth[0])
		}
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return httpErrorToStatus(resp.StatusCode, data)
	}
	if respMsg == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := c.unmarshal.Unmarshal(data, respMsg); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

func httpErrorToStatus(statusCode int, body []byte) error {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Code != "" {
		if code, ok := parseGRPCCode(payload.Code); ok {
			return status.Error(code, payload.Message)
		}
	}
	return status.Error(httpStatusToCode(statusCode), string(bytes.TrimSpace(body)))
}

func parseGRPCCode(raw string) (codes.Code, bool) {
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if code.String() == raw {
			return code, true
		}
	}
	return codes.Unknown, false
}

func httpStatusToCode(statusCode int) codes.Code {
	switch statusCode {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusPreconditionFailed:
		return codes.FailedPrecondition
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	default:
		return codes.Internal
	}
}
