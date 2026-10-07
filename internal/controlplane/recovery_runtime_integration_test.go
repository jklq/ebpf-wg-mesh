//go:build integration

package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestSharedRecoveryPauseFencesEveryReplicaAndBackgroundBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	url := createTestDatabase(t)
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyring := filepath.Join(t.TempDir(), "keys.json")
	if err := BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Provider().Close()
	signing := signkeys.New(db, keys.Registry())
	if err := signing.ResetForRecovery(ctx, "installation", "generation"); err != nil {
		t.Fatal(err)
	}
	bundle, err := signing.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(strings.TrimSpace(string(bundle))))
	sharedArchives := t.TempDir()
	var servers []*Server
	for range 2 {
		file := filepath.Join(t.TempDir(), "authority.json")
		// A replica's stale unpaused file cannot override shared recovery pause.
		admission := reconciliation.Authority{InstallationID: "installation", Generation: "generation", ClusterID: hex.EncodeToString(hash[:]), Paused: false}
		data, _ := json.Marshal(admission)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg := config.ControlPlaneConfig{AuthorityFile: file, Profile: config.ProfileDevelopment, Database: config.DatabaseConfig{URL: url}, StateDir: t.TempDir(), SecretKeys: config.SecretKeysConfig{KeyringPath: keyring},
			InternalGRPC: config.ListenerConfig{Listen: "127.0.0.1:0", TLS: config.ServerTLSConfig{ServerNames: []string{"localhost"}, BootstrapTokens: []config.AgentBootstrapToken{{AgentID: "new-agent", Token: "must-not-be-installed"}}, ServerCertValidityHours: 24, ClientCertValidityHours: 24}},
			Ingress:      config.IngressConfig{PublicAddr: "platform.local", XDSListen: "127.0.0.1:0"}, SourceArchives: config.SourceArchiveConfig{Provider: config.SourceArchiveProviderFile, Directory: sharedArchives}, ConsoleCallerIDs: []string{"recovery-console"},
			Dashboard: config.ManagedDashboardConfig{ServiceCallerID: "recovery-console"}, Bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{{ID: "operator", Email: "operator@example.test", Projects: []string{"must-remain-paused"}}}}, Mesh: testMeshConfig()}
		if err := config.FinalizeControlPlane(&cfg); err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !server.recoveryPaused {
			t.Fatal("replica ignored shared pause")
		}
		servers = append(servers, server)
		errCh := make(chan error, 1)
		go func() { errCh <- server.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			server.Close()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Error("paused replica failed to shut down")
			}
		})
	}
	for _, server := range servers {
		clientIdentity, err := server.EnsureDashboardClientIdentity(ctx, "recovery-console")
		if err != nil {
			t.Fatal(err)
		}
		conn := newDashboardPlatformClientConn(t, server.InternalAddr(), clientIdentity)
		client := platformv1.NewPlatformServiceClient(conn)
		userCtx := metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, server, "operator"))
		if _, err := client.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "must-not-exist"}); status.Code(err) != codes.Unavailable {
			t.Fatal("paused replica accepted public mutation", err)
		}
		if _, err := client.ListProjects(userCtx, &platformv1.ListProjectsRequest{}); err != nil {
			t.Fatal("paused operator inspection failed", err)
		}
		conn.Close()
	}
	var projects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 0 {
		t.Fatal("paused replica ran background/bootstrap mutations", projects, err)
	}
}
