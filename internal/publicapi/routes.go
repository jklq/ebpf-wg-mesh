package publicapi

import (
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// routeEntry describes one REST operation. The same table drives the mux,
// token scope confinement, and the generated OpenAPI document. req/resp
// name protobuf messages (platform.v1.*) whose JSON mapping is the wire
// body; empty resp means 204 No Content, empty req means no body.
type routeEntry struct {
	method  string
	pattern string
	summary string
	rule    scopeRule
	req     string
	resp    string
	handler http.HandlerFunc
}

func (s *Server) allRoutes() []routeEntry {
	var out []routeEntry
	out = append(out, s.metaRoutes()...)
	out = append(out, s.workspaceRoutes()...)
	out = append(out, s.projectRoutes()...)
	out = append(out, s.serviceRoutes()...)
	out = append(out, s.billingRoutes()...)
	return out
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "version": "v1"})
	})
	mux.HandleFunc("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		s.writeOpenAPI(w, r)
	})
	for _, entry := range s.allRoutes() {
		mux.HandleFunc(entry.method+" "+entry.pattern, s.authenticate(entry.rule, entry.handler))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, r, http.StatusNotFound, errorBody{Error: apiError{
			Code: "not_found", Message: "no such API route; see GET /v1/openapi.json", RequestID: requestID(r),
		}})
	})
	return mux
}

// respond writes a unary RPC result as the response body, or the mapped
// error. Empty results (emptypb.Empty) become 204.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, msg proto.Message, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, empty := msg.(*emptypb.Empty); empty || msg == nil {
		w.Header().Set("X-Request-Id", requestID(r))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeProto(w, r, status, msg)
}

func (s *Server) metaRoutes() []routeEntry {
	return []routeEntry{
		{method: "GET", pattern: "/v1/me", summary: "Identity behind an account-scoped token", rule: scopeRule{minScope: tierAccount}, resp: "", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			s.writeJSON(w, r, http.StatusOK, map[string]any{
				"user_id": token.UserID, "email": token.Email,
				"token": tokenInfo(token),
			})
		}},
		{method: "GET", pattern: "/v1/token", summary: "Introspect the presented token", resp: "", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			info := tokenInfo(token)
			info["user_id"] = token.UserID
			info["email"] = token.Email
			s.writeJSON(w, r, http.StatusOK, info)
		}},
	}
}

func tokenInfo(token Token) map[string]any {
	info := map[string]any{
		"id": token.ID, "name": token.Name, "scope": token.Scope, "access": token.Access,
	}
	if token.WorkspaceID != "" {
		info["workspace_id"] = token.WorkspaceID
	}
	if token.ProjectID != "" {
		info["project_id"] = token.ProjectID
	}
	if token.EnvironmentID != "" {
		info["environment_id"] = token.EnvironmentID
	}
	if token.ExpiresAt != nil {
		info["expires_at"] = token.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return info
}
