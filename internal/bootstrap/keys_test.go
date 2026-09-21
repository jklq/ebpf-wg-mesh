package bootstrap

import (
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
)

func TestKeysProvisionMintsVersionWithoutDatabase(t *testing.T) {
	t.Parallel()

	keyringPath := filepath.Join(t.TempDir(), "keys.json")
	if err := RunKeys([]string{"provision", "-secret-keys-keyring", keyringPath, "-key-id", "v1"}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	keyring, err := secretkeys.NewKeyring(keyringPath, secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	present, err := keyring.HasKeyMaterial(t.Context(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("provisioned version is not in the keyring file")
	}
	// Provisioning the same version again is refused.
	if err := RunKeys([]string{"provision", "-secret-keys-keyring", keyringPath, "-key-id", "v1"}); err == nil {
		t.Fatal("duplicate provision was accepted")
	}
}

func TestKeysProvisionGeneratesIDWhenEmpty(t *testing.T) {
	t.Parallel()

	keyringPath := filepath.Join(t.TempDir(), "keys.json")
	if err := RunKeys([]string{"provision", "-secret-keys-keyring", keyringPath}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	keyring, err := secretkeys.NewKeyring(keyringPath, secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	versions, err := keyring.LocalKeyVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || !strings.HasPrefix(versions[0], "kr-") {
		t.Fatalf("generated versions = %v", versions)
	}
}

func TestKeysUsageErrors(t *testing.T) {
	t.Parallel()

	if err := RunKeys(nil); err == nil {
		t.Fatal("empty args were accepted")
	}
	if err := RunKeys([]string{"rotate"}); err == nil {
		t.Fatal("removed rotate command was accepted")
	}
	// Database commands require a database URL.
	if err := RunKeys([]string{"list"}); err == nil || !strings.Contains(err.Error(), "db-url is required") {
		t.Fatalf("list without db = %v", err)
	}
	// Activate and delete require their key ID even before touching the DB.
	dbURL := "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"
	if err := RunKeys([]string{"activate", "-db-url", dbURL}); err == nil || !strings.Contains(err.Error(), "--key-id is required") {
		t.Fatalf("activate without key id = %v", err)
	}
	if err := RunKeys([]string{"delete", "-db-url", dbURL}); err == nil || !strings.Contains(err.Error(), "--key-id is required") {
		t.Fatalf("delete without key id = %v", err)
	}
}
