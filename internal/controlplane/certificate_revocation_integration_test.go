//go:build integration

package controlplane

import (
	"context"
	"math/big"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	identitycore "ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRevokedAgentMustRebootstrapWithFreshBoundToken(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	freshToken := config.AgentBootstrapToken{AgentID: "node-1", Token: "fresh-after-compromise"}
	if err := store.fleet.ensureAgentBootstrapTokens(context.Background(), []config.AgentBootstrapToken{freshToken}); err != nil {
		t.Fatalf("seed fresh bootstrap token: %v", err)
	}
	stateDir := t.TempDir()
	authority, err := NewTLSAuthority(context.Background(), config.ControlPlaneConfig{
		StateDir: stateDir,
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 6,
		}},
	}, ensureTestSigningKeys(t, store), identitycore.NewSharedCertificateRevocations(store.db))
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	service := newAgentService(store.fleet, nil, nil, nil, authority, nil, false, "", "")
	csr := string(mustCreateCSR(t, "node-1"))

	if err := authority.RevokeSerials([]string{"63"}); err != nil {
		t.Fatalf("revoke certificate in shared state: %v", err)
	}
	_, err = service.Enroll(
		contextWithCertificate(serviceCallerAgent, "node-1", big.NewInt(0x63)),
		&agentv1.EnrollRequest{AgentId: "node-1", CsrPem: csr},
	)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected revoked certificate renewal to fail, got %v", err)
	}

	resp, err := service.Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "node-1",
		CsrPem:         csr,
		BootstrapToken: freshToken.Token,
	})
	if err != nil {
		t.Fatalf("fresh unauthenticated bootstrap: %v", err)
	}
	if resp.GetCertPem() == "" {
		t.Fatal("expected replacement client certificate")
	}
	if _, err := service.Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "node-1",
		CsrPem:         csr,
		BootstrapToken: freshToken.Token,
	}); err != nil {
		t.Fatalf("same-key enrollment retry: %v", err)
	}
	otherCSR := string(mustCreateCSR(t, "node-1"))
	_, err = service.Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "node-1",
		CsrPem:         otherCSR,
		BootstrapToken: freshToken.Token,
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected consumed token to reject a different key, got %v", err)
	}
}
