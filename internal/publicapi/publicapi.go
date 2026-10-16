// Package publicapi is the platform's public HTTP API: a versioned
// REST/JSON surface under /v1 that external clients (CI, scripts, the MCP
// server) use with Bearer API tokens. It is a thin, stable translation
// layer over the control plane's PlatformService: request and response
// bodies are the protobuf JSON mapping of the underlying RPC messages, so
// the proto contract stays the single source of truth for field shapes.
//
// Authorization is two-layered. The token identifies its owning user and
// every call runs as that user, so workspace membership checks in the
// control plane apply unchanged. On top of that, this package confines
// workspace- and project-scoped tokens to their scope and confines
// read-access tokens to read routes.
package publicapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// Platform is the control-plane service the API delegates to. It is the
// generated PlatformService server interface; calls run in-process with a
// delegated-user context derived from the API token.
type Platform = platformv1.PlatformServiceServer

// Scope kinds for API tokens, mirroring the api_tokens scope column.
const (
	ScopeAccount   = "account"
	ScopeWorkspace = "workspace"
	ScopeProject   = "project"

	AccessRead = "read"
	AccessFull = "full"
)

// Token is an authenticated API token as the authenticator resolved it.
type Token struct {
	ID            string
	Name          string
	UserID        string
	Email         string
	Scope         string
	Access        string
	WorkspaceID   string
	ProjectID     string
	EnvironmentID string
	ExpiresAt     *time.Time
}

// ResourceScope says where a resource lives. Ancestor fields above the
// resource's own level are filled; fields below stay empty.
type ResourceScope struct {
	WorkspaceID   string
	ProjectID     string
	EnvironmentID string
	ServiceID     string
}

var (
	// ErrInvalidToken reports an unknown or malformed secret.
	ErrInvalidToken = errors.New("invalid api token")
	// ErrExpiredToken reports a token past its expiry.
	ErrExpiredToken = errors.New("api token expired")
	// ErrRevokedToken reports a revoked token.
	ErrRevokedToken = errors.New("api token revoked")
)

// TokenAuthenticator resolves presented secrets to tokens.
type TokenAuthenticator interface {
	Authenticate(ctx context.Context, secret string) (Token, error)
	// TouchLastUsed records token use (best-effort, throttled by the store).
	TouchLastUsed(ctx context.Context, tokenID string)
}

// ScopeResolver resolves resource ancestry for token scope confinement.
// kind is "workspace", "project", "environment", "service", "volume", or
// "domain" (by hostname). found is false for unknown resources.
type ScopeResolver interface {
	Resolve(ctx context.Context, kind, id string) (scope ResourceScope, found bool, err error)
}

// Config bounds the server. Zero rate limits select the defaults.
type Config struct {
	// RequestsPerHour caps requests per token per rolling hour.
	RequestsPerHour int
	// BurstPerSecond caps the per-token burst rate.
	BurstPerSecond int
	// MaxBodyBytes caps request bodies. Zero selects 1 MiB.
	MaxBodyBytes int64
}

const (
	defaultRequestsPerHour = 10_000
	defaultBurstPerSecond  = 50
	defaultMaxBodyBytes    = 1 << 20
)

type Server struct {
	platform Platform
	auth     TokenAuthenticator
	scopes   ScopeResolver
	limiter  *rateLimiter
	maxBody  int64
	handler  http.Handler
	now      func() time.Time
}

// New builds the API handler. All three dependencies are required.
func New(platform Platform, auth TokenAuthenticator, scopes ScopeResolver, cfg Config) *Server {
	if cfg.RequestsPerHour <= 0 {
		cfg.RequestsPerHour = defaultRequestsPerHour
	}
	if cfg.BurstPerSecond <= 0 {
		cfg.BurstPerSecond = defaultBurstPerSecond
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	s := &Server{
		platform: platform,
		auth:     auth,
		scopes:   scopes,
		limiter:  newRateLimiter(cfg.RequestsPerHour, cfg.BurstPerSecond),
		maxBody:  cfg.MaxBodyBytes,
		now:      time.Now,
	}
	s.handler = s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}
