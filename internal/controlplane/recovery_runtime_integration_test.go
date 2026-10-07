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

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/recovery"
	"ebof-wg-mesh/internal/testutil"
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
	waitForSingletonLease(t, servers[0])
	// Host administration admits an identity absent from the older SQL backup.
	// Ordinary enrollment is paused, but authenticated inventory must still work.
	material, err := identity.IssueClientCertificate(ctx, signing, identity.CallerAgent, "post-backup-host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn := newDashboardPlatformClientConn(t, servers[0].InternalAddr(), material)
	defer conn.Close()
	client := agentv1.NewAgentControlClient(conn)
	prior, err := client.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := prior.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{AgentId: "post-backup-host", InstallationId: "installation", RecoveryGeneration: "before"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := prior.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("prior generation reconnected through a new identity", err)
	}
	stream, err := client.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fleet := recovery.FleetHost{ID: "post-backup-host", Generation: "generation", Reachable: true, AuthorityResolved: true, Allocations: []recovery.FleetAllocation{{ID: "newer", SpecRevision: 900}}, Resources: []recovery.FleetResource{{Kind: "volume", ID: "unknown"}}}
	encoded, _ := json.Marshal(fleet)
	hello := &agentv1.AgentHello{AgentId: fleet.ID, InstallationId: "installation", RecoveryGeneration: fleet.Generation, ClusterId: hex.EncodeToString(hash[:]), RecoveryInventory: encoded}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(servers[0].cfg.StateDir, "recovery-inventory", "generation", "post-backup-host-fleet.json")
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 5 * time.Second}, func(context.Context) (bool, error) {
		data, err := os.ReadFile(file)
		return err == nil && string(data) == string(encoded), nil
	}); err != nil {
		t.Fatal("paused inventory was not independently recorded", err)
	}
	stream.CloseSend()
	var projects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 0 {
		t.Fatal("paused replica ran background/bootstrap mutations", projects, err)
	}
	var agents int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_registrations`).Scan(&agents); err != nil || agents != 0 {
		t.Fatal("inventory reporting changed restored desired state", agents, err)
	}
}
