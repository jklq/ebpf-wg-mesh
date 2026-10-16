//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/source"

	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/testdb"

	"github.com/google/uuid"
)

var (
	testServerOnce sync.Once
	testServer     *testdb.Server
	testServerErr  error

	testDatabaseOnce sync.Once
	testDatabaseURL  string
	testDatabaseErr  error
	testStoreMu      sync.Mutex
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testServer != nil {
		testServer.Stop()
	}
	os.Exit(code)
}

func upsertTestAgent(t *testing.T, store *persistence, ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	t.Helper()
	if hello == nil {
		return false, fmt.Errorf("agent hello is required")
	}
	if len(hello.RuntimeCapabilities) == 0 {
		hello.RuntimeCapabilities = []string{"containerd", "wireguard", "ebpf-policy"}
	}
	if strings.TrimSpace(hello.SoftwareVersion) == "" {
		hello.SoftwareVersion = "test"
	}
	if strings.TrimSpace(hello.SessionId) == "" {
		hello.SessionId = "test-session-" + hello.GetAgentId()
	}
	if strings.TrimSpace(hello.WireguardEndpoint) == "" {
		hello.WireguardEndpoint = "192.0.2.10:51820"
	}
	if hello.WireguardListenPort == 0 {
		hello.WireguardListenPort = 51820
	}
	if strings.TrimSpace(hello.AdvertiseAddr) == "" {
		hello.AdvertiseAddr = "fd00:30::"
	}
	if err := enrollTestAgent(ctx, store, hello); err != nil {
		return false, err
	}
	if err := store.db.QueryRowContext(ctx, `SELECT session_incarnation + 1 FROM agent_registrations WHERE id = $1`, hello.GetAgentId()).Scan(&hello.SessionIncarnation); err != nil {
		return false, err
	}
	return testDelivery(store).RegisterAgent(ctx, hello)
}

func enrollTestAgent(ctx context.Context, store *persistence, hello *agentv1.AgentHello) error {
	if hello.LocalStoreId == "" {
		hello.LocalStoreId = "test-store-" + hello.GetAgentId()
	}
	id := strings.TrimSpace(hello.GetAgentId())
	name := strings.TrimSpace(hello.GetName())
	if name == "" {
		name = id
	}
	failureDomain := strings.ToLower(id)
	now := time.Now().UTC()
	return store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_registrations(
			id, name, region, zone, failure_domain, reserved_cpu_millis, reserved_memory_mebibytes, created_at, updated_at
		) VALUES ($1, $2, 'default', '', $3, 0, 0, $4, $4) ON CONFLICT(id) DO NOTHING`, id, name, failureDomain, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
			VALUES ($1, 'enrolling', $2) ON CONFLICT(agent_id) DO NOTHING`, id, now); err != nil {
			return err
		}
		return nil
	})
}

func openTestStore(t *testing.T) *persistence {
	t.Helper()

	testStoreMu.Lock()
	t.Cleanup(testStoreMu.Unlock)

	dbURL := sharedTestDatabase(t)
	store, err := openPersistence(config.DatabaseConfig{
		URL:          dbURL,
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, testMeshConfig(), withSchemaInitialization())
	if err != nil {
		t.Fatal(err)
	}
	archiveStore, err := source.NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatalf("create source archive store: %v", err)
	}
	if err := store.source.ConfigureSourceArchives(archiveStore, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Each test gets an isolated development keyring; the active key is
	// bootstrapped after the reset wipes the shared database.
	provider, err := secretkeys.NewKeyring(filepath.Join(t.TempDir(), "keys.json"), secretkeys.KeyringOptions{AllowGenerate: true})
	if err != nil {
		t.Fatalf("create secret key provider: %v", err)
	}
	store.attachSecrets(secretkeys.New(store.db, provider))
	delivery := newDelivery(store, nil, nil, nil, nil)
	resetTestStore(t, store)
	if _, err := store.secrets.Registry().EnsureActiveKey(context.Background()); err != nil {
		t.Fatalf("ensure active envelope key: %v", err)
	}
	lease := newLeaseManager(store.database, time.Minute, time.Millisecond)
	leaseCtx, releaseLease, err := lease.hold(context.Background(), singletonLeaseName)
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.BecomeLive(leaseCtx); err != nil {
		releaseLease()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		delivery.ResignLive()
		releaseLease()
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return store
}

// ensureTestSigningKeys builds the shared signing-key inventory over a test
// store and ensures every scope, mirroring development server boot.
func ensureTestSigningKeys(t *testing.T, store *persistence) *signkeys.Service {
	t.Helper()
	svc := signkeys.New(store.db, store.secrets.Registry())
	ctx := context.Background()
	for _, scope := range signkeys.AllScopes() {
		if _, err := svc.EnsureActiveKey(ctx, scope, signkeys.EnsureOptions{}); err != nil {
			t.Fatalf("ensure signing scope %s: %v", scope, err)
		}
	}
	return svc
}

func sharedTestDatabase(t *testing.T) string {
	t.Helper()

	testDatabaseOnce.Do(func() {
		testDatabaseURL, testDatabaseErr = newTestDatabaseURL()
	})
	if testDatabaseErr != nil {
		t.Fatalf("create shared test database: %v", testDatabaseErr)
	}
	return testDatabaseURL
}

func resetTestStore(t *testing.T, store *persistence) {
	t.Helper()

	tables := []string{
		"billing_provider_events",
		"billing_notices",
		"billing_statement_lines",
		"billing_statements",
		"billing_credit_entries",
		"billing_periods",
		"billing_restrictions",
		"billing_accounts",
		"usage_intervals",
		"usage_hourly",
		"usage_resource_progress",
		"usage_producer_gaps",
		"usage_producers",
		"usage_attributions",
		"xds_node_observations",
		"ingress_nodes",
		"xds_publications",
		"control_plane_leases",
		"control_plane_storage",
		"environment_events",
		"deployment_actions",
		"project_github_repositories",
		"durable_work_items",
		"source_snapshots",
		"source_archive_objects",
		"registry_image_deletions",
		"source_revisions",
		"source_bindings",
		"github_repository_snapshots",
		"github_installation_repositories",
		"github_installations",
		"github_webhook_deliveries",
		"github_work_items",
		"build_runs",
		"builder_workers",
		"deployment_transitions",
		"allocation_assignments",
		"deployments",
		"service_rollouts",
		"service_delivery_status",
		"domain_bindings",
		"service_revisions",
		"services",
		"platform_signing_keys",
		"envelope_data_keys",
		"envelope_keys",
		"volume_destructions",
		"volumes",
		"environments",
		"projects",
		"workspace_invitations",
		"workspace_memberships",
		"workspaces",
		"agent_administration",
		"agent_registrations",
		"workload_ipv4_prefix_allocator",
		"agent_bootstrap_tokens",
		"agent_certificates",
		"platform_operators",
		"environment_network_identity_counter",
	}
	if err := store.withProductTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(context.Background(), `UPDATE agent_authority SET epoch = 1, outstanding_not_after = '1970-01-01' WHERE id = 1`); err != nil {
			return err
		}
		for _, table := range tables {
			if _, err := tx.ExecContext(context.Background(), `DELETE FROM `+table); err != nil {
				return fmt.Errorf("clear %s: %w", table, err)
			}
		}
		if _, err := tx.ExecContext(context.Background(), `
			INSERT INTO environment_network_identity_counter(id, next_identity)
			VALUES (TRUE, 1)
		`); err != nil {
			return fmt.Errorf("reset environment network identity counter: %w", err)
		}
		if _, err := tx.ExecContext(context.Background(), `
			INSERT INTO workload_ipv4_prefix_allocator(id, pool_cidr, prefix_bits, next_ordinal)
			VALUES (TRUE, $1, $2, 0)
		`, store.mesh.WorkloadIPv4PoolCIDR, store.mesh.WorkloadIPv4NodePrefixBits); err != nil {
			return fmt.Errorf("reset IPv4 prefix allocator: %w", err)
		}
		if _, err := tx.ExecContext(context.Background(), `UPDATE build_scheduler_control SET paused = FALSE`); err != nil {
			return fmt.Errorf("reset build scheduler pause: %w", err)
		}
		if _, err := tx.ExecContext(context.Background(), `UPDATE billing_control SET collection_paused = FALSE, pause_reason = ''`); err != nil {
			return fmt.Errorf("reset billing collection pause: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reset test database: %v", err)
	}
}

func createTestDatabase(t *testing.T) string {
	t.Helper()
	dbURL, err := newTestDatabaseURL()
	if err != nil {
		t.Fatal(err)
	}
	return dbURL
}

func productionEnvironmentID(t *testing.T, store *persistence, projectID string) string {
	t.Helper()
	environment, err := store.catalog.productionEnvironmentByProjectInternal(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load production environment for project %s: %v", projectID, err)
	}
	return environment.ID
}

func newTestDatabaseURL() (string, error) {
	raw := os.Getenv("CONTROLPLANE_TEST_DATABASE_URL")
	if raw == "" {
		ts, err := getSharedTestServer()
		if err != nil {
			return "", err
		}
		raw = ts.PGURL().String()
	}
	pgURL, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	adminDB, err := platformdb.OpenURL(context.Background(), raw, "public")
	if err != nil {
		return "", fmt.Errorf("open admin database: %w", err)
	}

	defer adminDB.Close()

	dbName := "cp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), `CREATE DATABASE `+dbName); err != nil {
		return "", fmt.Errorf("CREATE DATABASE %s: %w", dbName, err)
	}

	pgURL.Path = "/" + dbName
	return platformdb.WithSchema(pgURL.String(), platformdb.SchemaPlatform)
}

func sharedTestServer(t *testing.T) *testdb.Server {
	t.Helper()
	ts, err := getSharedTestServer()
	if err != nil {
		t.Fatalf("start test PostgreSQL: %v", err)
	}
	return ts
}

func getSharedTestServer() (*testdb.Server, error) {
	testServerOnce.Do(func() {
		testServer, testServerErr = testdb.Start("")
	})
	if testServerErr != nil {
		return nil, testServerErr
	}
	return testServer, nil
}

func cloneURL(source *url.URL) (*url.URL, error) {
	if source == nil {
		return nil, fmt.Errorf("nil test database URL")
	}
	clone := *source
	query := clone.Query()
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone, nil
}

// grantWorkspaceRole gives userID role in the workspace that owns projectID,
// bypassing invitations.
func grantWorkspaceRole(t *testing.T, store *persistence, ctx context.Context, projectID, userID, role string) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO workspace_memberships(workspace_id, user_id, role, created_at)
		 SELECT workspace_id, $2, $3, now() FROM projects WHERE id = $1
		 ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = excluded.role`,
		projectID, userID, role); err != nil {
		t.Fatal(err)
	}
}
