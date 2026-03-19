//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"ebof-wg-mesh/internal/config"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
	"github.com/google/uuid"
)

const cockroachTestVersion = "v26.1.0"

var (
	testServerOnce sync.Once
	testServer     testserver.TestServer
	testServerErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testServer != nil {
		testServer.Stop()
	}
	os.Exit(code)
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	dbURL := createTestDatabase(t)
	store, err := OpenStore(config.DatabaseConfig{
		URL:          dbURL,
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return store
}

func createTestDatabase(t *testing.T) string {
	t.Helper()

	ts := sharedTestServer(t)
	adminDB, err := sql.Open("pgx", ts.PGURL().String())
	if err != nil {
		t.Fatalf("sql.Open admin db: %v", err)
	}
	defer adminDB.Close()

	dbName := "cp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}

	pgURL := cloneURL(t, ts.PGURL())
	pgURL.Path = "/" + dbName
	return pgURL.String()
}

func sharedTestServer(t *testing.T) testserver.TestServer {
	t.Helper()

	testServerOnce.Do(func() {
		testServer, testServerErr = testserver.NewTestServer(
			testserver.CustomVersionOpt(cockroachTestVersion),
		)
	})
	if testServerErr != nil {
		t.Fatalf("NewTestServer: %v", testServerErr)
	}
	return testServer
}

func cloneURL(t *testing.T, source *url.URL) *url.URL {
	t.Helper()

	if source == nil {
		t.Fatal("nil CockroachDB test URL")
	}
	clone := *source
	query := clone.Query()
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone
}
