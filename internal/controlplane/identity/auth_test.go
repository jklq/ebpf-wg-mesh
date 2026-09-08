package identity

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestInternalAuthAllowsDashboardPlatformCallsWithDelegatedUser(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	ctx := contextWithClientIdentity(CallerDashboard, "dashboard-1")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		userAssertionHeader, signedUserAssertion(t, "user-1", nil),
	))

	authorized, err := authz.authorizeGRPCContext(ctx, "/platform.v1.PlatformService/ListProjects")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	user, err := DelegatedUserFromContext(authorized)
	if err != nil {
		t.Fatalf("DelegatedUserFromContext: %v", err)
	}
	if user.UserID != "user-1" {
		t.Fatalf("unexpected delegated user ID %q", user.UserID)
	}
}

func TestInternalAuthRejectsMissingDelegatedUserOnPlatformCall(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	_, err := authz.authorizeGRPCContext(contextWithClientIdentity(CallerDashboard, "dashboard-1"), "/platform.v1.PlatformService/ListProjects")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestInternalAuthRejectsAgentCallingPlatformService(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	ctx := contextWithClientIdentity(CallerAgent, "agent-1")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		userAssertionHeader, signedUserAssertion(t, "user-1", nil),
	))

	_, err := authz.authorizeGRPCContext(ctx, "/platform.v1.PlatformService/ListProjects")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthRejectsDashboardCallingAgentSync(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	_, err := authz.authorizeGRPCContext(contextWithClientIdentity(CallerDashboard, "dashboard-1"), "/agent.v1.AgentControl/Sync")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthAllowsDashboardOpsCalls(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	_, err := authz.authorizeGRPCContext(contextWithClientIdentity(CallerDashboard, "dashboard-1"), "/platform.v1.OpsService/IngestGitHubWebhook")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
}

func TestInternalAuthRejectsUnauthenticatedOpsCalls(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	_, err := authz.authorizeGRPCContext(context.Background(), "/platform.v1.OpsService/IngestGitHubWebhook")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthRejectsWrongClassOpsCalls(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	_, err := authz.authorizeGRPCContext(contextWithClientIdentity(CallerBuilder, "builder-1"), "/platform.v1.OpsService/IngestGitHubWebhook")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthRejectsNonCanonicalAndUnknownMethods(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	ctx := contextWithClientIdentity(CallerDashboard, "dashboard-1")
	for _, method := range []string{
		"platform.v1.BuilderService/ClaimBuild",
		"/unknown.v1.Service/Method",
	} {
		_, err := authz.authorizeGRPCContext(ctx, method)
		if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unimplemented {
			t.Fatalf("expected method %q to be rejected, got %v", method, err)
		}
	}
}

func TestInternalAuthRejectsUnpinnedDashboard(t *testing.T) {
	t.Parallel()

	authz := newTestInternalAuth()
	ctx := contextWithClientIdentity(CallerDashboard, "dashboard-2")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(userAssertionHeader, signedUserAssertion(t, "user-1", nil)))
	_, err := authz.authorizeGRPCContext(ctx, "/platform.v1.PlatformService/ListProjects")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected unpinned dashboard CN to be rejected, got %v", err)
	}
}

func TestInternalAuthRejectsInvalidUserAssertions(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*jwt.RegisteredClaims){
		"issuer": func(claims *jwt.RegisteredClaims) {
			claims.Issuer = "other-console"
		},
		"audience": func(claims *jwt.RegisteredClaims) {
			claims.Audience = jwt.ClaimStrings{"other-service"}
		},
		"expired": func(claims *jwt.RegisteredClaims) {
			claims.IssuedAt = jwt.NewNumericDate(testAssertionNow.Add(-time.Minute))
			claims.ExpiresAt = jwt.NewNumericDate(testAssertionNow.Add(-30 * time.Second))
		},
		"excessive lifetime": func(claims *jwt.RegisteredClaims) {
			claims.ExpiresAt = jwt.NewNumericDate(testAssertionNow.Add(time.Minute))
		},
		"missing id": func(claims *jwt.RegisteredClaims) {
			claims.ID = ""
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := contextWithClientIdentity(CallerDashboard, "dashboard-1")
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
				userAssertionHeader, signedUserAssertion(t, "user-1", mutate),
			))
			_, err := newTestInternalAuth().authorizeGRPCContext(ctx, "/platform.v1.PlatformService/ListProjects")
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("expected invalid %s assertion to be rejected, got %v", name, err)
			}
		})
	}
}

func TestInternalAuthRejectsInvalidSignatureAndDuplicateAssertions(t *testing.T) {
	t.Parallel()

	valid := signedUserAssertion(t, "user-1", nil)
	tests := map[string][]string{
		"signature": {valid[:len(valid)-1] + differentLastCharacter(valid)},
		"duplicate": {valid, valid},
	}
	for name, values := range tests {
		name, values := name, values
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := contextWithClientIdentity(CallerDashboard, "dashboard-1")
			ctx = metadata.NewIncomingContext(ctx, metadata.MD{userAssertionHeader: values})
			_, err := newTestInternalAuth().authorizeGRPCContext(ctx, "/platform.v1.PlatformService/ListProjects")
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("expected invalid %s assertion to be rejected, got %v", name, err)
			}
		})
	}
}

func differentLastCharacter(value string) string {
	if strings.HasSuffix(value, "x") {
		return "y"
	}
	return "x"
}

func TestServiceCallerFromContextFallsBackToAuthenticatedPeerIdentity(t *testing.T) {
	t.Parallel()

	caller, err := ServiceCallerFromContext(contextWithClientIdentity(CallerBuilder, "builder-1"))
	if err != nil {
		t.Fatalf("ServiceCallerFromContext: %v", err)
	}
	if caller.Class != CallerBuilder || caller.ID != "builder-1" {
		t.Fatalf("unexpected caller %+v", caller)
	}
}

func contextWithClientIdentity(class CallerClass, id string) context.Context {
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:         id,
			OrganizationalUnit: []string{string(class)},
		},
	}
	return context.WithValue(
		context.Background(),
		verifiedClientCertificateContextKey{},
		cert,
	)
}

const testUserAssertionSecret = "test-control-plane-user-assertion-secret"

var testAssertionNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func newTestInternalAuth() *InternalAuth {
	authz := NewInternalAuth("dashboard-1", testUserAssertionSecret)
	authz.now = func() time.Time { return testAssertionNow }
	return authz
}

func signedUserAssertion(t *testing.T, userID string, mutate func(*jwt.RegisteredClaims)) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		Issuer:    userAssertionIssuer,
		Audience:  jwt.ClaimStrings{userAssertionAudience},
		Subject:   userID,
		ExpiresAt: jwt.NewNumericDate(testAssertionNow.Add(userAssertionMaxAge)),
		IssuedAt:  jwt.NewNumericDate(testAssertionNow),
		ID:        "assertion-1",
	}
	if mutate != nil {
		mutate(&claims)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testUserAssertionSecret))
	if err != nil {
		t.Fatalf("sign user assertion: %v", err)
	}
	return token
}
