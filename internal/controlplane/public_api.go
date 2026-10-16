package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/publicapi"
	"ebof-wg-mesh/internal/telemetry"
)

// publicAPITokens adapts the api_tokens store to the public API's
// authenticator.
type publicAPITokens struct{ db *database }

func (a publicAPITokens) Authenticate(ctx context.Context, secret string) (publicapi.Token, error) {
	rec, err := a.db.apiTokenBySecret(ctx, secret, time.Now().UTC())
	switch {
	case errors.Is(err, errAPITokenInvalid):
		return publicapi.Token{}, publicapi.ErrInvalidToken
	case errors.Is(err, errAPITokenExpired):
		return publicapi.Token{}, publicapi.ErrExpiredToken
	case errors.Is(err, errAPITokenRevoked):
		return publicapi.Token{}, publicapi.ErrRevokedToken
	case err != nil:
		return publicapi.Token{}, err
	}
	return publicapi.Token{
		ID: rec.ID, Name: rec.Name,
		UserID: rec.CreatedByUserID, Email: rec.CreatedByEmail,
		Scope: rec.Scope, Access: rec.Access,
		WorkspaceID: rec.WorkspaceID, ProjectID: rec.ProjectID, EnvironmentID: rec.EnvironmentID,
		ExpiresAt: rec.ExpiresAt,
	}, nil
}

func (a publicAPITokens) TouchLastUsed(ctx context.Context, tokenID string) {
	if err := a.db.touchAPITokenLastUsed(ctx, tokenID); err != nil {
		slog.DebugContext(ctx, "touch api token last_used_at failed", "error", err, "token_id", tokenID)
	}
}

// publicAPIScopes adapts resource ancestry resolution.
type publicAPIScopes struct{ db *database }

func (r publicAPIScopes) Resolve(ctx context.Context, kind, id string) (publicapi.ResourceScope, bool, error) {
	scope, found, err := r.db.resolveResourceScope(ctx, kind, id)
	if err != nil || !found {
		return publicapi.ResourceScope{}, found, err
	}
	return publicapi.ResourceScope{
		WorkspaceID: scope.WorkspaceID, ProjectID: scope.ProjectID,
		EnvironmentID: scope.EnvironmentID, ServiceID: scope.ServiceID,
	}, true, nil
}

// newPublicAPIHandler assembles the public API over the in-process
// platform service. Recovery pause blocks mutations exactly like the
// internal transports: GETs keep serving restored state.
func newPublicAPIHandler(platform *platformService, store *database, cfg config.PublicAPIConfig, paused bool) http.Handler {
	api := publicapi.New(platform, publicAPITokens{db: store}, publicAPIScopes{db: store}, publicapi.Config{
		RequestsPerHour: cfg.RequestsPerHour,
		BurstPerSecond:  cfg.BurstPerSecond,
	})
	handler := telemetry.HTTPHandler(api, "public-api")
	if !paused {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"recovery pauses mutations","request_id":""}}` + "\n"))
			return
		}
		handler.ServeHTTP(w, r)
	})
}
