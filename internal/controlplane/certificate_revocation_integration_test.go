//go:build integration

package controlplane

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRevokedAgentMustRebootstrapWithFreshBoundToken(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	freshToken := config.AgentBootstrapToken{AgentID: "node-1", Token: "fresh-after-compromise"}
	if err := store.ensureAgentBootstrapTokens(context.Background(), []config.AgentBootstrapToken{freshToken}); err != nil {
		t.Fatalf("seed fresh bootstrap token: %v", err)
	}
	stateDir := t.TempDir()
	revocationPath := filepath.Join(stateDir, "revoked.txt")
	authority, err := NewTLSAuthority(config.ControlPlaneConfig{
		StateDir: stateDir,
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:                  []string{"controlplane"},
			ServerCertValidityHours:      24,
			ClientCertValidityHours:      6,
			RevokedClientCertSerialsFile: revocationPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	service := NewAgentService(store, nil, nil, nil, authority, nil, false, "", "")
	csr := string(mustCreateCSR(t, "node-1"))

	if err := os.WriteFile(revocationPath, []byte("63\n"), 0o600); err != nil {
		t.Fatalf("write revocation file: %v", err)
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
	_, err = service.Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "node-1",
		CsrPem:         csr,
		BootstrapToken: freshToken.Token,
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected replacement token to remain single-use, got %v", err)
	}
}
