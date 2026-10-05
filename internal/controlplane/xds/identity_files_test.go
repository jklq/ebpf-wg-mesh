package xds

import (
	"os"
	"path/filepath"
	"testing"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/identity/identitytest"
)

func TestIdentityRotationIsAtomicAndRetainsLastGoodCredentials(t *testing.T) {
	t.Parallel()
	pki := identitytest.New(t)
	dir := filepath.Join(t.TempDir(), "identity with spaces")
	first := pki.Material(t, identity.CallerIngress, "envoy-1")
	if err := WriteIdentity(dir, dir, first); err != nil {
		t.Fatal(err)
	}
	old, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	second := pki.Material(t, identity.CallerIngress, "envoy-1")
	if err := WriteIdentity(dir, dir, second); err != nil {
		t.Fatal(err)
	}
	current, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current == old {
		t.Fatal("rotation reused the credential generation")
	}
	if _, err := ClientTLS(dir, "controlplane"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "current", "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions = %o", info.Mode().Perm())
	}
	second.KeyPEM = first.KeyPEM
	if err := WriteIdentity(dir, dir, second); err == nil {
		t.Fatal("accepted a torn certificate/key pair")
	}
	after, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if after != current {
		t.Fatal("failed rotation replaced last good credentials")
	}
}
