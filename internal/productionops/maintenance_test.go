package productionops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

func TestIssuedCredentialsSurviveIndependentCompleterFailure(t *testing.T) {
	r := testRunner(t)
	a := r.Plan.Installation.Hosts[0]
	a.Trusted = true
	a.Reliability = "reliable"
	a.FailureDomain = "first"
	b := a
	b.ID = "b"
	b.Binding.ServerID = "b"
	b.FailureDomain = "second"
	r.Plan.Installation.Hosts = []deploy.Host{a, b}
	r.Plan.AdministrationHost = "a"
	r.Config.CompletionHosts = []string{"a", "b"}
	root := r.Config.StateDirectory
	paths := []string{filepath.Join(root, r.Plan.Generation, "agent-client.json"), filepath.Join(root, r.Plan.Generation, "envoy-ingress", "generation", "client.key"), filepath.Join(r.databasePKI(), "component.json")}
	for _, path := range paths {
		if err := writePrivate(path, []byte("issued private material")); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, r.Plan.Generation, "envoy-ingress", "current")
	if err := os.Symlink("generation", link); err != nil {
		t.Fatal(err)
	}
	remote := inventoryPeerRemote{Root: filepath.Join(root, "peers"), Source: filepath.Join(root, "inventory.json"), Offline: map[string]bool{"a": true}}
	r.Remote = remote
	if err := r.replicateCredentialInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		copied := strings.Replace(path, root, filepath.Join(remote.Root, "b"), 1)
		data, err := os.ReadFile(copied)
		if err != nil || string(data) != "issued private material" {
			t.Fatal("surviving completer lost issuance", err)
		}
		info, err := os.Stat(copied)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("issuance copy is not private", err)
		}
	}
	if target, err := os.Readlink(strings.Replace(link, root, filepath.Join(remote.Root, "b"), 1)); err != nil || target != "generation" {
		t.Fatal("surviving ingress cache lost its selected identity", err)
	}
	remote.Offline["b"] = true
	r.Remote = remote
	if r.replicateCredentialInputs(context.Background()) == nil {
		t.Fatal("missing all independently retained issuance accepted")
	}
}
