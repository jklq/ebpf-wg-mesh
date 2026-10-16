package publicapi

import (
	"context"
	"net/http"

	"ebof-wg-mesh/internal/controlplane/identity"

	"github.com/google/uuid"
)

type tokenContextKey struct{}

func tokenFromContext(ctx context.Context) (Token, bool) {
	token, ok := ctx.Value(tokenContextKey{}).(Token)
	return token, ok
}

// Scope tiers for scopeRule.minScope, broadest token scope required.
const (
	tierAny       = ""          // account, workspace, and project tokens
	tierWorkspace = "workspace" // account and workspace tokens (project/environment lifecycle)
	tierAccount   = "account"   // account tokens only (identity and membership management)
)

// scopeRule declares how a route is confined for scoped tokens.
type scopeRule struct {
	minScope string
	// kind and param name a path parameter whose resource ancestry must
	// contain the token's scope. Empty kind means no path confinement;
	// handlers then confine body/query parents themselves.
	kind  string
	param string
	// write marks routes that read-access tokens may not call.
	write bool
}

// requestMiddleware assigns a request ID, authenticates the Bearer token,
// applies rate limits, enforces the route's scope rule, and installs the
// delegated-user context PlatformService calls require.
func (s *Server) authenticate(rule scopeRule, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id))

		secret, ok := bearerSecret(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api"`)
			s.writeError(w, r, unauthorized("missing bearer token: send Authorization: Bearer <api token>"))
			return
		}
		token, err := s.auth.Authenticate(r.Context(), secret)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api", error="invalid_token"`)
			s.writeError(w, r, err)
			return
		}
		decision := s.limiter.allow(token.ID, s.now())
		s.limiter.writeRateHeaders(w, decision)
		if !decision.allowed {
			s.writeError(w, r, &httpError{status: http.StatusTooManyRequests, code: "rate_limited", message: "rate limit exceeded; retry after the Retry-After interval"})
			return
		}
		s.auth.TouchLastUsed(r.Context(), token.ID)
		if err := s.checkRule(r, token, rule); err != nil {
			s.writeError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), tokenContextKey{}, token)
		ctx = identity.WithDelegatedUserEmail(ctx, token.UserID, token.Email)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) checkRule(r *http.Request, token Token, rule scopeRule) error {
	if rule.write && token.Access == AccessRead {
		return forbiddenScope("this token has read-only access")
	}
	switch rule.minScope {
	case tierAccount:
		if token.Scope != ScopeAccount {
			return forbiddenScope("this operation requires an account-scoped token")
		}
	case tierWorkspace:
		if token.Scope == ScopeProject {
			return forbiddenScope("this operation requires an account- or workspace-scoped token")
		}
	}
	if rule.kind == "" {
		return nil
	}
	resourceID := r.PathValue(rule.param)
	if resourceID == "" {
		return badRequest("missing " + rule.param)
	}
	return s.checkResourceScope(r.Context(), token, rule.kind, resourceID)
}

// checkResourceScope confines a scoped token to its workspace or
// project+environment by resolving the resource's ancestry. Unknown
// resources pass through: the control plane answers with its own
// not-found or denial, indistinguishable by design.
func (s *Server) checkResourceScope(ctx context.Context, token Token, kind, resourceID string) error {
	if token.Scope == ScopeAccount || resourceID == "" {
		return nil
	}
	scope, found, err := s.scopes.Resolve(ctx, kind, resourceID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return checkScope(token, scope)
}

// checkScope is the pure confinement decision over a resolved ancestry.
func checkScope(token Token, scope ResourceScope) error {
	switch token.Scope {
	case ScopeWorkspace:
		if scope.WorkspaceID != token.WorkspaceID {
			return forbiddenScope("this token is scoped to a different workspace")
		}
	case ScopeProject:
		if scope.WorkspaceID != token.WorkspaceID || scope.ProjectID != token.ProjectID ||
			(scope.EnvironmentID != "" && scope.EnvironmentID != token.EnvironmentID) {
			return forbiddenScope("this token is scoped to a different project environment")
		}
	}
	return nil
}

// checkWorkspaceScope confines list/body workspace selectors: a scoped
// token may only name its own workspace (empty means its own).
func checkWorkspaceSelector(token Token, workspaceID string) error {
	if token.Scope == ScopeAccount || workspaceID == "" {
		return nil
	}
	if workspaceID != token.WorkspaceID {
		return forbiddenScope("this token is scoped to a different workspace")
	}
	return nil
}
