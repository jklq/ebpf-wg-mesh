//go:build integration

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/testdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

// Exercise the pinned native command contract against TLS, not just CSV
// fixtures. A healthy single-node database remains valid without redundancy.
func TestSecureNativeDatabaseInspection(t *testing.T) {
	ts, err := testserver.NewTestServer(
		testserver.CustomVersionOpt(testdb.DefaultVersion),
		testserver.SecureOpt(),
		testserver.StoreOnDiskOpt(),
		testserver.CacheSizeOpt(0.02),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.Stop)
	if err := ts.WaitForInit(); err != nil {
		t.Fatal(err)
	}
	u := ts.PGURL()
	i, r, inv := fixture(1)
	i.Hosts[0].Network.Address = u.Hostname()
	p := build(t, i, r, State{}, inv, false)
	path := filepath.Join(t.TempDir(), "plan.json")
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var output bytes.Buffer
	err = RunDatabaseStatus(ctx, []string{
		"--plan", path,
		"--binary", filepath.Join(os.TempDir(), "cockroach-"+testdb.DefaultVersion),
		"--host", u.Host,
		"--certs-dir", filepath.Dir(u.Query().Get("sslcert")),
		"--require-convergence",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	var status DatabaseStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Members) != 1 || status.Members[0] != "a" || status.Replicated || len(status.Ranges) == 0 {
		t.Fatalf("incorrect native single-node assessment: %+v", status)
	}
}
