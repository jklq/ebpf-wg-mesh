//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testAPITokenService(store *persistence) *platformService {
	return newPlatformService(store.platform(), noopNotifier{}, noopIngress{},
		newTestDelivery(store, noopNotifier{}, noopIngress{}, nil),
		withAPITokens(store.database, authz.NewAuthorizer(store.db)))
}

func TestAPITokenLifecycle(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, environmentID, _, _, _ := seedAuthzProject(t, store, ctx)
	service := testAPITokenService(store)
	editor := identity.WithDelegatedUserEmail(context.Background(), "editor", "editor@example.com")

	created, err := service.CreateAPIToken(editor, &platformv1.CreateAPITokenRequest{
		Name: "mcp laptop", Scope: platformv1.APITokenScope_API_TOKEN_SCOPE_PROJECT,
		ProjectId: projectID, EnvironmentId: environmentID,
		Access: platformv1.APITokenAccess_API_TOKEN_ACCESS_READ, ExpiresInDays: 30,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if !looksLikeAPITokenSecret(created.GetSecret()) {
		t.Fatalf("secret has the wrong shape: %q", created.GetSecret())
	}
	token := created.GetToken()
	if token.GetScope() != platformv1.APITokenScope_API_TOKEN_SCOPE_PROJECT ||
		token.GetAccess() != platformv1.APITokenAccess_API_TOKEN_ACCESS_READ ||
		token.GetProjectId() != projectID || token.GetEnvironmentId() != environmentID ||
		token.GetWorkspaceId() == "" || token.GetExpiresAt() == nil {
		t.Fatalf("unexpected token: %+v", token)
	}

	// The public API authenticator resolves the secret exactly once
	// through the hash; the RPC views never carry the secret again.
	rec, err := store.database.apiTokenBySecret(ctx, created.GetSecret(), time.Now())
	if err != nil {
		t.Fatalf("apiTokenBySecret: %v", err)
	}
	if rec.ID != token.GetId() || rec.CreatedByUserID != "editor" || rec.CreatedByEmail != "editor@example.com" {
		t.Fatalf("authenticated record = %+v", rec)
	}
	if _, err := store.database.apiTokenBySecret(ctx, created.GetSecret()+"x", time.Now()); !errors.Is(err, errAPITokenInvalid) {
		t.Fatalf("mutated secret = %v, want errAPITokenInvalid", err)
	}

	listed, err := service.ListAPITokens(editor, &platformv1.ListAPITokensRequest{})
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(listed.GetTokens()) != 1 || listed.GetTokens()[0].GetId() != token.GetId() {
		t.Fatalf("listed = %+v", listed.GetTokens())
	}

	// Scope resolution pins the token's environment to its project.
	scope, found, err := store.database.resolveResourceScope(ctx, "environment", environmentID)
	if err != nil || !found {
		t.Fatalf("resolveResourceScope: found=%v err=%v", found, err)
	}
	if scope.ProjectID != projectID || scope.WorkspaceID != token.GetWorkspaceId() {
		t.Fatalf("scope = %+v", scope)
	}

	// A viewer cannot mint project tokens (write required) and cannot
	// revoke someone else's token.
	viewer := contextWithDelegatedUser("viewer", "")
	if _, err := service.CreateAPIToken(viewer, &platformv1.CreateAPITokenRequest{
		Name: "sneaky", Scope: platformv1.APITokenScope_API_TOKEN_SCOPE_PROJECT,
		ProjectId: projectID, EnvironmentId: environmentID,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer create = %v, want PermissionDenied", err)
	}
	if _, err := service.RevokeAPIToken(viewer, &platformv1.RevokeAPITokenRequest{TokenId: token.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("viewer revoke = %v, want NotFound", err)
	}

	if _, err := service.RevokeAPIToken(editor, &platformv1.RevokeAPITokenRequest{TokenId: token.GetId()}); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, err := store.database.apiTokenBySecret(ctx, created.GetSecret(), time.Now()); !errors.Is(err, errAPITokenRevoked) {
		t.Fatalf("revoked secret = %v, want errAPITokenRevoked", err)
	}
	listed, err = service.ListAPITokens(editor, &platformv1.ListAPITokensRequest{IncludeRevoked: true})
	if err != nil || len(listed.GetTokens()) != 1 || listed.GetTokens()[0].GetRevokedAt() == nil {
		t.Fatalf("listed after revoke = %+v, err %v", listed.GetTokens(), err)
	}
}

func TestAPITokenWorkspaceScopeRequiresAdmin(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, _, _, _, _ := seedAuthzProject(t, store, ctx)
	project, err := store.catalog.projectByID(ctx, testUser("owner"), projectID)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := project.WorkspaceID
	service := testAPITokenService(store)

	// "editor" is a member, not an admin, of the seeded workspace.
	if _, err := service.CreateAPIToken(contextWithDelegatedUser("editor", ""), &platformv1.CreateAPITokenRequest{
		Name: "team ci", Scope: platformv1.APITokenScope_API_TOKEN_SCOPE_WORKSPACE, WorkspaceId: workspaceID,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("member workspace token = %v, want PermissionDenied", err)
	}
	admin, err := service.CreateAPIToken(contextWithDelegatedUser("owner", ""), &platformv1.CreateAPITokenRequest{
		Name: "team ci", Scope: platformv1.APITokenScope_API_TOKEN_SCOPE_WORKSPACE, WorkspaceId: workspaceID,
	})
	if err != nil {
		t.Fatalf("admin workspace token: %v", err)
	}
	if admin.GetToken().GetAccess() != platformv1.APITokenAccess_API_TOKEN_ACCESS_FULL {
		t.Fatalf("default access = %v, want full", admin.GetToken().GetAccess())
	}
	// Expired tokens fail closed.
	rec, err := store.database.apiTokenByID(ctx, admin.GetToken().GetId())
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	if _, err := store.db.ExecContext(ctx, `UPDATE api_tokens SET expires_at = statement_timestamp() - interval '1 minute' WHERE id = $1`, admin.GetToken().GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.database.apiTokenBySecret(ctx, admin.GetSecret(), time.Now()); !errors.Is(err, errAPITokenExpired) {
		t.Fatalf("expired secret = %v, want errAPITokenExpired", err)
	}
}
