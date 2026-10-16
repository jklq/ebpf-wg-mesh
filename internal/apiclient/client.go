// Package apiclient is the Go client for the platform public API
// (internal/publicapi). Request and response bodies are the protobuf
// JSON mapping, so callers work with platformv1 messages directly.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Client talks to one public API base URL with one API token.
type Client struct {
	BaseURL string
	Token   string
	// HTTPClient defaults to a client with a 60s timeout.
	HTTPClient *http.Client
	// UserAgent is sent on every request; empty selects the default.
	UserAgent string
}

// New builds a client. baseURL is the API origin, e.g.
// "https://api.example.test" (paths are added under /v1).
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// APIError is a non-2xx API response in the public error shape.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d (%s): %s [request_id=%s]", e.Status, e.Code, e.Message, e.RequestID)
}

// IsCode reports whether err is an APIError with the given public code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

var (
	marshalOpts   = protojson.MarshalOptions{UseProtoNames: true}
	unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// do performs one request. req may be nil (no body), a proto.Message, or a
// value for encoding/json. resp may be nil, a proto.Message to fill, or a
// pointer for encoding/json. GET requests retry once on 429/5xx.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, req, resp any) error {
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		retry, err := c.doOnce(ctx, method, path, query, req, resp)
		if err == nil {
			return nil
		}
		lastErr = err
		if method != http.MethodGet || !retry {
			return err
		}
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, query url.Values, req, resp any) (retry bool, err error) {
	var body io.Reader
	if req != nil {
		data, err := encodeBody(req)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(data)
	}
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return false, err
	}
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "ebpf-wg-mesh-apiclient"
	}
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept", "application/json")
	if req != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return true, fmt.Errorf("public api request: %w", err)
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 32<<20))
	if err != nil {
		return true, fmt.Errorf("read public api response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		apiErr := parseAPIError(httpResp.StatusCode, data, httpResp.Header.Get("X-Request-Id"))
		return httpResp.StatusCode == 429 || httpResp.StatusCode >= 500, apiErr
	}
	if resp == nil || len(bytes.TrimSpace(data)) == 0 {
		return false, nil
	}
	if msg, ok := resp.(proto.Message); ok {
		if err := unmarshalOpts.Unmarshal(data, msg); err != nil {
			return false, fmt.Errorf("decode public api response: %w", err)
		}
		return false, nil
	}
	if err := json.Unmarshal(data, resp); err != nil {
		return false, fmt.Errorf("decode public api response: %w", err)
	}
	return false, nil
}

func encodeBody(req any) ([]byte, error) {
	if msg, ok := req.(proto.Message); ok {
		return marshalOpts.Marshal(msg)
	}
	return json.Marshal(req)
}

func parseAPIError(status int, data []byte, headerRequestID string) *APIError {
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	apiErr := &APIError{Status: status, Code: "http_error", Message: strings.TrimSpace(string(data)), RequestID: headerRequestID}
	if json.Unmarshal(data, &body) == nil && body.Error.Code != "" {
		apiErr.Code = body.Error.Code
		apiErr.Message = body.Error.Message
		if body.Error.RequestID != "" {
			apiErr.RequestID = body.Error.RequestID
		}
	}
	if len(apiErr.Message) > 512 {
		apiErr.Message = apiErr.Message[:512]
	}
	return apiErr
}

// TokenInfo is GET /v1/token (and the token object inside /v1/me).
type TokenInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Scope         string `json:"scope"`
	Access        string `json:"access"`
	WorkspaceID   string `json:"workspace_id"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	ExpiresAt     string `json:"expires_at"`
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
}

// Me is GET /v1/me.
type Me struct {
	UserID string    `json:"user_id"`
	Email  string    `json:"email"`
	Token  TokenInfo `json:"token"`
}

func query(values map[string]string) url.Values {
	out := url.Values{}
	for key, value := range values {
		if value != "" {
			out.Set(key, value)
		}
	}
	return out
}

func boolQuery(v bool) string {
	if !v {
		return ""
	}
	return "true"
}

func intQuery(v int32) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(int64(v), 10)
}

func escape(segment string) string { return url.PathEscape(segment) }
