package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestInternalAuthAllowsDashboardPlatformCallsWithDelegatedUser(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	ctx := contextWithClientIdentity(serviceCallerDashboard, "dashboard-1")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		delegatedUserSubjectHeader, "user-1",
		delegatedUserEmailHeader, "user@example.com",
	))

	authorized, err := authz.authorize(ctx, "/platform.v1.PlatformService/ListProjects", false)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	user, err := DelegatedUserFromContext(authorized)
	if err != nil {
		t.Fatalf("DelegatedUserFromContext: %v", err)
	}
	if user.Subject != "user-1" {
		t.Fatalf("unexpected delegated subject %q", user.Subject)
	}
}

func TestInternalAuthRejectsMissingDelegatedUserOnPlatformCall(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	_, err := authz.authorize(contextWithClientIdentity(serviceCallerDashboard, "dashboard-1"), "/platform.v1.PlatformService/ListProjects", false)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestInternalAuthRejectsAgentCallingPlatformService(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	ctx := contextWithClientIdentity(serviceCallerAgent, "agent-1")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		delegatedUserSubjectHeader, "user-1",
		delegatedUserEmailHeader, "user@example.com",
	))

	_, err := authz.authorize(ctx, "/platform.v1.PlatformService/ListProjects", false)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthRejectsDashboardCallingAgentSync(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	_, err := authz.authorize(contextWithClientIdentity(serviceCallerDashboard, "dashboard-1"), "/agent.v1.AgentControl/Sync", true)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthAllowsDashboardOpsCalls(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	_, err := authz.authorize(contextWithClientIdentity(serviceCallerDashboard, "dashboard-1"), "/platform.v1.OpsService/IngestGitHubWebhook", false)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
}

func TestInternalAuthRejectsUnauthenticatedOpsCalls(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	_, err := authz.authorize(context.Background(), "/platform.v1.OpsService/IngestGitHubWebhook", false)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestInternalAuthRejectsWrongClassOpsCalls(t *testing.T) {
	t.Parallel()

	authz := NewInternalAuth()
	_, err := authz.authorize(contextWithClientIdentity(serviceCallerBuilder, "builder-1"), "/platform.v1.OpsService/IngestGitHubWebhook", false)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestServiceCallerFromContextFallsBackToAuthenticatedPeerIdentity(t *testing.T) {
	t.Parallel()

	caller, err := ServiceCallerFromContext(contextWithClientIdentity(serviceCallerBuilder, "builder-1"))
	if err != nil {
		t.Fatalf("ServiceCallerFromContext: %v", err)
	}
	if caller.Class != serviceCallerBuilder || caller.ID != "builder-1" {
		t.Fatalf("unexpected caller %+v", caller)
	}
}

func contextWithClientIdentity(class serviceCallerClass, id string) context.Context {
	cert := &x509.Certificate{
		Subject: pkix.Name{
			CommonName:         id,
			OrganizationalUnit: []string{string(class)},
		},
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{cert}},
			},
		},
	})
}
