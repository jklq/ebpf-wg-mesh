package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
)

// TestActiveCAIdentityMatchesServer pins the cross-process contract: the
// agent hashes the bundle's first certificate exactly as the control plane
// hashes its active CA, so enrollment converges on the same identity on
// both sides, including through a rotation.
func TestActiveCAIdentityMatchesServer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	keys := signkeystest.New(t)
	serverIdentity := func() string {
		t.Helper()
		active, err := keys.Active(ctx, signkeys.ScopeInternalCA)
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		digest := sha256.Sum256(bytes.TrimSpace(active.Record.PublicPEM))
		return hex.EncodeToString(digest[:])
	}
	agentIdentity := func() string {
		t.Helper()
		bundle, err := keys.PublicBundle(ctx, signkeys.ScopeInternalCA)
		if err != nil {
			t.Fatalf("PublicBundle: %v", err)
		}
		id, err := activeCAIdentity(bundle)
		if err != nil {
			t.Fatalf("activeCAIdentity: %v", err)
		}
		return id
	}
	if agentIdentity() != serverIdentity() {
		t.Fatal("steady-state identity differs between agent and server")
	}
	keys.Rotate(t, signkeys.ScopeInternalCA)
	if agentIdentity() != serverIdentity() {
		t.Fatal("overlap identity differs between agent and server")
	}
	if _, err := activeCAIdentity(nil); err == nil {
		t.Fatal("empty bundle was accepted")
	}
	if _, err := activeCAIdentity([]byte("not-pem")); err == nil {
		t.Fatal("non-PEM bundle was accepted")
	}
}

func TestClusterIdentityAdoptAndRequire(t *testing.T) {
	t.Parallel()

	store, err := openLocalStateStore(t.TempDir(), "node-1")
	if err != nil {
		t.Fatalf("openLocalStateStore: %v", err)
	}
	defer store.Close()
	if got := store.clusterIdentity(); got != "" {
		t.Fatalf("fresh store pins %q, want empty", got)
	}
	// Initial enrollment pins the observed identity.
	if err := store.requireClusterIdentity("cluster-a"); err != nil {
		t.Fatalf("requireClusterIdentity: %v", err)
	}
	if err := store.adoptClusterIdentity("cluster-a"); err != nil {
		t.Fatalf("adoptClusterIdentity: %v", err)
	}
	if got := store.clusterIdentity(); got != "cluster-a" {
		t.Fatalf("pinned %q, want cluster-a", got)
	}
	// Renewal over the authenticated channel adopts the rotated identity.
	if err := store.adoptClusterIdentity("cluster-b"); err != nil {
		t.Fatalf("adoptClusterIdentity: %v", err)
	}
	if got := store.clusterIdentity(); got != "cluster-b" {
		t.Fatalf("pinned %q, want cluster-b", got)
	}
	if err := store.adoptClusterIdentity(""); err == nil {
		t.Fatal("empty adoption was accepted")
	}
	// A genuinely different authority still trips recovery.
	if err := store.requireClusterIdentity("cluster-other"); err == nil {
		t.Fatal("mismatched require was accepted")
	}
}
