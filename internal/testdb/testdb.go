package testdb

import (
	"net/url"
	"strings"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

const DefaultVersion = "v26.1.0"

const EnvVar = "DASHBOARD_TEST_DATABASE_URL"

func Start(version string) (testserver.TestServer, error) {
	if strings.TrimSpace(version) == "" {
		version = DefaultVersion
	}
	return testserver.NewTestServer(testserver.CustomVersionOpt(version))
}

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
