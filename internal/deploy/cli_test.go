package deploy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestRecoveryAfterPermanentHostLossRecordsCutoffAndResumes(t *testing.T) {
	i, r, inv := fixture(1)
	state := applied(build(t, i, r, State{}, inv, false))
	state.Bindings["a"] = i.Hosts[0].Binding
	interrupted := build(t, i, r, state, inv, false)
	state.Progress = &Progress{PlanID: interrupted.ID, Plan: &interrupted, Started: map[string]bool{}, Completed: map[string]Evidence{}}
	store, key, statePath := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery uses a separately identified physical host. The old machine is
	// permanently lost, so only external fencing can prove it stopped serving.
	i.Hosts[0].ID = "rescue"
	i.Hosts[0].Binding = Binding{Provider: "imported", ServerID: "replacement-machine"}
	i.ManagementHost = "rescue"
	for name, storage := range i.Storage {
		storage.Hosts = []string{"rescue"}
		i.Storage[name] = storage
	}
	for n := range i.Endpoints {
		i.Endpoints[n].Hosts = []string{"rescue"}
	}
	status := inv.Hosts["a"]
	status.ServerID = "replacement-machine"
	inv.Hosts = map[string]HostStatus{"rescue": status}
	inv.Database = DatabaseStatus{}

	dir := t.TempDir()
	manifest, bundle, keyFile := filepath.Join(dir, "production.yaml"), filepath.Join(dir, "release.yaml"), filepath.Join(dir, "key")
	for path, value := range map[string]any{manifest: i, bundle: r} {
		data, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	cutoff := testNow.Add(-time.Hour)
	driver := fake(inv)
	driver.failHook = "restore"
	var output bytes.Buffer
	err := Run(context.Background(), []string{"restore", "--installation", manifest, "--bundle", bundle, "--key-file", keyFile, "--state", statePath, "--backup", "s3://backups/production/points/production/complete?versionId=protected", "--data-loss-cutoff", cutoff.Format(time.RFC3339)}, &output, driver)
	if err == nil || !strings.Contains(output.String(), cutoff.Format(time.RFC3339)) {
		t.Fatal("restoration interruption/cutoff was not reported", err, output.String())
	}
	store, err = OpenState(statePath, key)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := store.Read()
	store.Close()
	if err != nil || partial.Progress == nil || partial.LastBackup.DataLossCutoff != cutoff {
		t.Fatal("restoration intent was not durably recorded", err)
	}
	plan := partial.Progress.Plan
	if plan.Previous == nil || plan.Previous.Interrupted == nil || plan.Previous.Bindings["a"].ServerID != "1" || len(plan.Previous.Placements) == 0 {
		t.Fatal("fencing cannot identify previous instances and infrastructure")
	}
	fenced := false
	for _, op := range plan.Operations {
		if op.Hook == "recovery-fence" {
			_, fenced = partial.Progress.Completed[op.ID]
		}
		if op.Host == "a" {
			t.Fatal("recovery required SSH to the permanently lost host", op)
		}
		if op.Hook == "restore" && !fenced {
			t.Fatal("destructive restore preceded verified independent fencing")
		}
	}
	driver.failHook = ""
	if err := Run(context.Background(), []string{"resume", "--key-file", keyFile, "--state", statePath}, &output, driver); err != nil {
		t.Fatal(err)
	}
	store, err = OpenState(statePath, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	final, err := store.Read()
	if err != nil || final.Progress != nil || final.Policy.ManagementHost != "rescue" || final.LastRestore == nil || final.LastRestore.DataLossCutoff != cutoff {
		t.Fatal("recovery did not complete on the alternative inventory", err)
	}
}
