//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// redirectConn follows explicit live-owner redirects between known replicas,
// mirroring the stress fixture's client. A mutation transport failure is not
// retried; only a FailedPrecondition redirect with a known peer is followed.
type redirectConn struct {
	grpc.ClientConnInterface
	peers map[string]grpc.ClientConnInterface
}

func (c redirectConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn := c.ClientConnInterface
	for attempt := 0; attempt <= len(c.peers); attempt++ {
		err := conn.Invoke(ctx, method, args, reply, opts...)
		if status.Code(err) != codes.FailedPrecondition {
			return err
		}
		addr, ok := deliverycore.ParseLiveOwnerRedirect(status.Convert(err).Message())
		if !ok {
			return err
		}
		next, ok := c.peers[addr]
		if !ok {
			return fmt.Errorf("live owner redirected outside the fixture: %q", addr)
		}
		conn = next
	}
	return status.Error(codes.Unavailable, "live owner redirect loop")
}

// TestNonOwnerReadRedirectsToOwnerOverGRPC guards that reads served from
// owner-local live allocation state redirect a standby replica to the live
// owner instead of failing with Internal or hanging until the RPC deadline.
func TestNonOwnerReadRedirectsToOwnerOverGRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner := startSystemControlPlane(t, systemControlPlaneOptions{withDashboard: true})
	standby := startSystemControlPlane(t, systemControlPlaneOptions{
		databaseURL: owner.cfg.Database.URL,
		stateDir:    owner.cfg.StateDir,
		standby:     true,
	})

	if held, _, err := standby.server.leases.Lookup(ctx, SingletonLeaseName); err != nil || held {
		t.Fatalf("standby replica holds the singleton lease: held=%v err=%v", held, err)
	}

	identity, err := owner.server.EnsureDashboardClientIdentity(systemTestDashboardID)
	if err != nil {
		t.Fatalf("EnsureDashboardClientIdentity: %v", err)
	}
	ownerConn := newDashboardPlatformClientConn(t, owner.server.InternalAddr(), identity)
	defer ownerConn.Close()
	standbyConn := newDashboardPlatformClientConn(t, standby.server.InternalAddr(), identity)
	defer standbyConn.Close()

	peers := map[string]grpc.ClientConnInterface{
		owner.server.InternalAddr():   ownerConn,
		standby.server.InternalAddr(): standbyConn,
	}
	ownerClient := platformv1.NewPlatformServiceClient(ownerConn)
	redirectClient := platformv1.NewPlatformServiceClient(redirectConn{ClientConnInterface: standbyConn, peers: peers})
	standbyRaw := platformv1.NewPlatformServiceClient(standbyConn)

	delegatedCtx := metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, "user-1"))
	project, err := ownerClient.CreateProject(delegatedCtx, &platformv1.CreateProjectRequest{Name: "redirect-test"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	environments, err := ownerClient.ListEnvironments(delegatedCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(environments.GetEnvironments()) != 1 {
		t.Fatalf("expected one environment, got %d", len(environments.GetEnvironments()))
	}
	environmentID := environments.GetEnvironments()[0].GetId()

	// A raw standby connection must itself redirect, so removing the
	// requireLiveOwner gate cannot pass this test via the auto-following client.
	if _, err := standbyRaw.ListServices(delegatedCtx, &platformv1.ListServicesRequest{EnvironmentId: environmentID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("standby ListServices without redirect = %v, want FailedPrecondition", err)
	} else if addr, ok := deliverycore.ParseLiveOwnerRedirect(status.Convert(err).Message()); !ok || addr != owner.server.InternalAddr() {
		t.Fatalf("standby ListServices redirect = %q, want owner %q", status.Convert(err).Message(), owner.server.InternalAddr())
	}
	if _, err := standbyRaw.GetServiceStatus(delegatedCtx, &platformv1.GetServiceStatusRequest{ServiceId: "missing"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("standby GetServiceStatus without redirect = %v, want FailedPrecondition", err)
	}

	listed, err := redirectClient.ListServices(delegatedCtx, &platformv1.ListServicesRequest{EnvironmentId: environmentID})
	if err != nil {
		t.Fatalf("standby ListServices: %v", err)
	}
	if listed.GetIndex() == 0 {
		t.Fatal("standby ListServices returned zero index")
	}
	if len(listed.GetServices()) != 0 {
		t.Fatalf("expected empty service list, got %d", len(listed.GetServices()))
	}

	if _, err := redirectClient.GetService(delegatedCtx, &platformv1.GetServiceRequest{ServiceId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("standby GetService missing = %v, want NotFound", err)
	}
	if _, err := redirectClient.GetServiceStatus(delegatedCtx, &platformv1.GetServiceStatusRequest{ServiceId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("standby GetServiceStatus missing = %v, want NotFound", err)
	}
}
