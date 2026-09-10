//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
	"github.com/google/uuid"
)

const cockroachTestVersion = "v26.1.0"

var (
	testServerOnce sync.Once
	testServer     testserver.TestServer
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
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_registrations(
		id, name, region, zone, failure_domain, reserved_cpu_millis, reserved_memory_mebibytes, created_at, updated_at
	) VALUES ($1, $2, 'default', '', $3, 0, 0, $4, $4) ON CONFLICT(id) DO NOTHING`, id, name, failureDomain, now); err != nil {
		return err
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
		VALUES ($1, 'enrolling', $2) ON CONFLICT(agent_id) DO NOTHING`, id, now); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO agent_presence(agent_id, session_id, last_observation_sequence, last_contact_at, ready, reachable, updated_at)
		VALUES ($1, '', 0, $2, FALSE, FALSE, $2) ON CONFLICT(agent_id) DO NOTHING`, id, time.Unix(0, 0).UTC())
	return err
}

func openTestStore(t *testing.T) *persistence {
	t.Helper()

	// Most integration tests only need isolated data, not an independently
	// migrated database. Reuse one database and hold the lease until the test's
	// cleanup runs; rebuilding the full CockroachDB schema for every parallel
	// test makes the suite spend minutes contending on DDL.
	testStoreMu.Lock()
	t.Cleanup(testStoreMu.Unlock)

	dbURL := sharedTestDatabase(t)
	store, err := openPersistence(config.DatabaseConfig{
		URL:          dbURL,
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	archiveStore, err := source.NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatalf("create source archive store: %v", err)
	}
	store.source.ConfigureSourceArchives(archiveStore)
	newDelivery(store, nil, nil, nil, nil)
	resetTestStore(t, store)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return store
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

	// TRUNCATE is a schema change in CockroachDB and takes roughly a second even
	// for empty tables. These tables hold only a handful of test rows, so ordered
	// deletes are substantially faster.
	tables := []string{
		"sandbox_profile_audit_events",
		"control_plane_leases",
		"control_plane_storage",
		"environment_events",
		"deployment_actions",
		"project_github_repositories",
		"source_work_items",
		"source_snapshots",
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
		"allocation_observations",
		"allocation_assignments",
		"deployments",
		"service_rollouts",
		"domain_bindings",
		"service_revisions",
		"services",
		"volumes",
		"project_memberships",
		"environments",
		"projects",
		"agent_presence",
		"agent_administration",
		"agent_registrations",
		"workload_ipv4_prefix_allocator",
		"agent_bootstrap_tokens",
		"agent_certificates",
		"platform_operators",
		"environment_network_identity_counter",
	}
	if err := store.withTx(context.Background(), func(tx *sql.Tx) error {
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
	ts, err := getSharedTestServer()
	if err != nil {
		return "", err
	}

	adminDB, err := sql.Open("pgx", ts.PGURL().String())
	if err != nil {
		return "", fmt.Errorf("sql.Open admin db: %w", err)
	}
	defer adminDB.Close()

	dbName := "cp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), `CREATE DATABASE `+dbName); err != nil {
		return "", fmt.Errorf("CREATE DATABASE %s: %w", dbName, err)
	}

	pgURL, err := cloneURL(ts.PGURL())
	if err != nil {
		return "", err
	}
	pgURL.Path = "/" + dbName
	return pgURL.String(), nil
}

func sharedTestServer(t *testing.T) testserver.TestServer {
	t.Helper()
	ts, err := getSharedTestServer()
	if err != nil {
		t.Fatalf("NewTestServer: %v", err)
	}
	return ts
}

func getSharedTestServer() (testserver.TestServer, error) {
	testServerOnce.Do(func() {
		testServer, testServerErr = testserver.NewTestServer(
			testserver.CustomVersionOpt(cockroachTestVersion),
		)
	})
	if testServerErr != nil {
		return nil, testServerErr
	}
	return testServer, nil
}

func cloneURL(source *url.URL) (*url.URL, error) {
	if source == nil {
		return nil, fmt.Errorf("nil CockroachDB test URL")
	}
	clone := *source
	query := clone.Query()
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone, nil
}
