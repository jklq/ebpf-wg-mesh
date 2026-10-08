package productionops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

// Each available peer executes the actual copy and checksum script in a
// separate filesystem. An unavailable peer performs no operation.
type inventoryPeerRemote struct {
	Root, Source string
	Offline      map[string]bool
}

func (r inventoryPeerRemote) Run(ctx context.Context, i deploy.Installation, h deploy.Host, script string) ([]byte, error) {
	if r.Offline[h.ID] {
		return nil, fmt.Errorf("host %s unreachable", h.ID)
	}
	target := filepath.Join(r.Root, h.ID)
	return exec.CommandContext(ctx, "sh", "-eu", "-c", portableHostScript(strings.ReplaceAll(script, filepath.Dir(r.Source), target))).Output()
}
func (r inventoryPeerRemote) Upload(context.Context, deploy.Installation, deploy.Host, string, string, string) error {
	return fmt.Errorf("unused upload")
}

func TestIndependentCompletersRetainActualInventoryThroughPeerFailure(t *testing.T) {
	r := testRunner(t)
	r.Plan.Installation.OneHostFailure = true
	a := r.Plan.Installation.Hosts[0]
	a.Trusted, a.Reliability, a.FailureDomain = true, "reliable", "first"
	b := a
	b.ID, b.Binding.ServerID, b.FailureDomain = "b", "b", "second"
	r.Plan.Installation.Hosts = []deploy.Host{a, b}
	r.Config.CompletionHosts = []string{"a", "b"}
	path := filepath.Join(r.Config.StateDirectory, "inventory.json")
	r.Plan.Installation.Recovery.Inventory = path
	remote := inventoryPeerRemote{Root: filepath.Join(r.Config.StateDirectory, "peers"), Source: path, Offline: map[string]bool{}}
	r.Remote = remote
	evidence := []byte(`{"observed":[{"id":"unreachable-agent","reachable":false,"containers":[{"id":"preserve-me"}]}]}`)
	if err := writePrivate(path, evidence); err != nil {
		t.Fatal(err)
	}
	if err := r.replicateInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote.Offline["a"] = true
	r.Remote = remote
	if err := r.replicateInventory(context.Background()); err != nil {
		t.Fatal("unreachable peer prevented independent completion", err)
	}
	for _, id := range []string{"a", "b"} {
		got, err := os.ReadFile(filepath.Join(remote.Root, id, "inventory.json"))
		if err != nil || string(got) != string(evidence) {
			t.Fatal("resource evidence was lost on", id, err)
		}
	}
	remote.Offline["b"] = true
	if err := r.replicateInventory(context.Background()); err == nil {
		t.Fatal("no available copy was accepted as verified")
	}
}
