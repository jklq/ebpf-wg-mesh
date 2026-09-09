package testdb

import (
	"net/url"
	"strings"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

// DefaultVersion pins the CockroachDB version used by test harnesses.
const DefaultVersion = "v26.1.0"

// EnvVar carries the test database URL to a wrapped child process.
const EnvVar = "DASHBOARD_TEST_DATABASE_URL"

// Start launches an ephemeral CockroachDB test server. An empty version
// selects DefaultVersion.
func Start(version string) (testserver.TestServer, error) {
	if strings.TrimSpace(version) == "" {
		version = DefaultVersion
	}
	return testserver.NewTestServer(testserver.CustomVersionOpt(version))
}

// NormalizeURL returns a copy of source with sslmode=disable when unset.
// It returns nil when source is nil.
func NormalizeURL(source *url.URL) *url.URL {
	if source == nil {
		return nil
	}
	clone := *source
	query := clone.Query()
	if strings.TrimSpace(query.Get("sslmode")) == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone
}
