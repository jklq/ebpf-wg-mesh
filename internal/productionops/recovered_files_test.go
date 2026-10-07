package productionops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func TestHydrateIndependentInstallerAfterOriginalDiskLoss(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "lost-disk")
	destination := filepath.Join(t.TempDir(), "recovered")
	independentKey := filepath.Join(t.TempDir(), "offsite-key")
	if err := writePrivate(independentKey, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	config := Config{Version: 1, StateDirectory: origin + "/runtime", RecoveryConfig: origin + "/recovery.json", Database: DatabaseConfig{Binary: origin + "/cockroach", CertificateDirectory: origin + "/pki", URLFile: origin + "/database-url", Name: "platform", BackupURIFile: origin + "/backup-uri"}, Console: ConsoleConfig{AdminBinary: origin + "/console-admin", Schema: "dashboard", TokenKeyFile: origin + "/console.key"}, WildcardCertificate: origin + "/wildcard.crt", WildcardKey: origin + "/wildcard.key", PlatformDomain: "example.com", InternalServerName: "core.example.com"}
	rec := recovery.Config{Storage: recovery.StorageConfig{CredentialsFile: origin + "/storage-credentials"}, RecoveryKeyFile: origin + "/independent.key", KeyringFile: origin + "/keyring.json", Images: recovery.Images{AuthFile: origin + "/registry-auth"}}
	i := deploy.Installation{ID: "production", Release: "r43", OperationsConfig: origin + "/operations.json", Secrets: map[string]deploy.SecretRef{"ssh": {File: origin + "/ssh.key"}, "recovery": {File: origin + "/independent.key"}}, Backup: deploy.Backup{RecoveryKey: "recovery"}, Hosts: []deploy.Host{{ID: "a", SSH: deploy.SSH{KnownHosts: origin + "/known_hosts"}}}, Recovery: deploy.Recovery{Inventory: origin + "/inventory.json"}}
	bundle := map[string][]byte{origin + "/ssh.key": []byte("private-ssh-identity"), origin + "/known_hosts": []byte("pinned-known-host"), rec.Storage.CredentialsFile: []byte("storage-credentials"), rec.Images.AuthFile: []byte("registry-credentials"), config.Database.BackupURIFile: []byte("s3://independent"), config.WildcardCertificate: []byte("certificate"), config.WildcardKey: []byte("private-key")}
	bundle[config.RecoveryConfig], _ = json.Marshal(rec)
	bundle[i.OperationsConfig], _ = json.Marshal(config)
	result := recoveredFiles{Installation: destination + "/installation.json", Release: destination + "/release.json", Secrets: destination + "/bundle.json", Keyring: destination + "/keyring.json", ConsoleKeys: []string{destination + "/token.key"}, Tools: map[string]string{}}
	for path, value := range map[string]any{result.Installation: i, result.Secrets: bundle, result.Keyring: map[string]any{"version": 1, "keys": map[string]any{}}} {
		if err := saveJSON(path, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePrivate(result.ConsoleKeys[0], []byte("independent-console-token")); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"operations", "cockroachdb", "console-admin", "aws", "skopeo"} {
		path := destination + "/tools/" + tool
		if err := writePrivate(path, []byte("#!/bin/sh\nexit 0\n")); err != nil {
			t.Fatal(err)
		}
		result.Tools["r43/tool/"+tool+"/"+runtime.GOARCH] = path
	}
	if err := os.MkdirAll(origin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	originalState := deploy.State{Version: 1, InstallationID: i.ID, Generation: "prior", Policy: &i, Bindings: map[string]deploy.Binding{"a": {Provider: "linux", ServerID: "durable-id"}}, Bundle: &deploy.Release{ID: "r43"}}
	if err := hydrateRecovered(context.Background(), recovery.Config{RecoveryKeyFile: independentKey}, recovery.Point{}, &originalState, &result, destination); err != nil {
		t.Fatal(err)
	}
	key := mustReadUnit(t, result.StateKey)
	store, err := deploy.OpenState(result.State, key)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := store.Read()
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if actual.Bindings["a"].ServerID != "durable-id" || actual.Generation != "prior" {
		t.Fatal("independent external inventory was lost")
	}
	if string(mustReadUnit(t, actual.Policy.Secrets["ssh"].File)) != "private-ssh-identity" {
		t.Fatal("recovered SSH identity differs")
	}
	if string(mustReadUnit(t, actual.Policy.Hosts[0].SSH.KnownHosts)) != "pinned-known-host" {
		t.Fatal("known-host trust was not recovered")
	}
	var selected Config
	if err := privateJSON(result.OperationsConfig, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Database.Binary != "/opt/ebpf-wg-mesh/production/r43/tools/cockroachdb" {
		t.Fatal("remote tools are not bound to protected release")
	}
	var remote recovery.Config
	if err := privateJSON(selected.RecoveryConfig, &remote); err != nil {
		t.Fatal(err)
	}
	if remote.RecoveryKeyFile != independentKey || remote.KeyringFile != result.Keyring {
		t.Fatal("independent wrapping key closure is broken")
	}
	var operator recovery.Config
	if err := privateJSON(result.RecoveryConfig, &operator); err != nil {
		t.Fatal(err)
	}
	if operator.Storage.Binary != result.Tools["r43/tool/aws/"+runtime.GOARCH] {
		t.Fatal("operator still depends on lost native tools")
	}
	for _, path := range actual.Policy.OperationsInputs {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("input cannot be staged: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(origin); !os.IsNotExist(err) {
		t.Fatal("recovery recreated the lost disk")
	}
}
func mustReadUnit(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublicProbeUsesExactOrigin(t *testing.T) {
	if sameEndpoint("https://console.example.com.attacker.invalid/readyz", "https://console.example.com") {
		t.Fatal("hostname prefix accepted")
	}
	if sameEndpoint("https://console.example.com:444/readyz", "https://console.example.com") {
		t.Fatal("port mismatch accepted")
	}
	if !sameEndpoint("https://console.example.com/readyz", "https://console.example.com") {
		t.Fatal("valid path rejected")
	}
}
func TestRedundantCompletionNeedsIndependentTrustedHosts(t *testing.T) {
	r := testRunner(t)
	r.Plan.Installation.OneHostFailure = true
	r.Plan.Installation.Hosts[0].Trusted = true
	r.Plan.Installation.Hosts[0].Reliability = "reliable"
	r.Plan.Installation.Hosts[0].FailureDomain = "site/a"
	r.Config.CompletionHosts = []string{"a"}
	if _, err := r.completionHosts(); err == nil {
		t.Fatal("one completer passed redundant requirement")
	}
	h := r.Plan.Installation.Hosts[0]
	h.ID = "b"
	r.Plan.Installation.Hosts = append(r.Plan.Installation.Hosts, h)
	r.Config.CompletionHosts = []string{"a", "b"}
	if _, err := r.completionHosts(); err == nil {
		t.Fatal("shared completer failure domain accepted")
	}
	r.Plan.Installation.Hosts[1].FailureDomain = "site/b"
	if _, err := r.completionHosts(); err != nil {
		t.Fatal(err)
	}
	r.Plan.Installation.Hosts[1].Trusted = false
	if _, err := r.completionHosts(); err == nil {
		t.Fatal("untrusted host received recovery authority")
	}
}
