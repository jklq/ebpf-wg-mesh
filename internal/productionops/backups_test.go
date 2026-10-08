package productionops

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func TestProtectionSnapshotDoesNotOverwriteAppliedPlan(t *testing.T) {
	r := testRunner(t)
	r.Plan.Recovery = true // Restored native keys are already independently provisioned.
	r.Plan.Release.Tools = map[string]map[string]deploy.Artifact{"operations": {runtime.GOARCH: {SHA256: strings.Repeat("a", 64)}}}
	r.Plan.Installation.Release = r.Plan.Release.ID
	r.Config.RecoveryConfig = filepath.Join(r.Config.StateDirectory, "selected-recovery.json")
	r.Config.Console.TokenKeyFile = filepath.Join(r.Config.StateDirectory, "console.key")
	r.Plan.Installation.OperationsConfig = filepath.Join(r.Config.StateDirectory, "operations.json")
	c := recovery.Config{Storage: recovery.StorageConfig{Bucket: "isolated", Prefix: "test", Region: "test", Owner: "test", CredentialsFile: filepath.Join(r.Config.StateDirectory, "credentials"), Profile: "test", Account: "recovery", PrimaryAccount: "primary", FailureDomain: "recovery", PrimaryDomains: []string{"primary"}, WriterPrincipal: "test"}, RecoveryKeyFile: filepath.Join(r.Config.StateDirectory, "recovery.key"), KeyringFile: filepath.Join(r.Config.StateDirectory, "keyring.json"), Images: recovery.Images{Binary: "/prior-release/skopeo"}}
	for path, data := range map[string][]byte{c.RecoveryKeyFile: []byte(strings.Repeat("r", 32)), c.Storage.CredentialsFile: []byte("isolated-service-selection"), r.Config.Console.TokenKeyFile: []byte("independent-console-key")} {
		if err := writePrivate(path, data); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := secretkeys.NewKeyring(c.KeyringFile, secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GenerateKey(context.Background(), "protected-key"); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(r.Config.RecoveryConfig, c); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(r.Plan.Installation.OperationsConfig, r.Config); err != nil {
		t.Fatal(err)
	}
	oldPolicy, oldBundle := r.Plan.Installation, r.Plan.Release
	oldPolicy.Release, oldPolicy.ManagementHost, oldBundle.ID = "r42", "old-admin", "r42"
	previous := deploy.State{Version: 1, Policy: &oldPolicy, Bundle: &oldBundle, Bindings: map[string]deploy.Binding{"a": {Provider: "linux", ServerID: "protected-binding"}}}
	statePath := filepath.Join(r.Config.StateDirectory, "previous-state.json")
	if err := saveJSON(statePath, previous); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLATFORM_DEPLOYMENT_STATE", statePath)
	before := deploy.Digest(r.Plan)
	for attempts := 0; attempts < 2; attempts++ {
		if err := r.prepareProtection(context.Background(), &r.Plan.Release); err != nil {
			t.Fatal(err)
		}
		if deploy.Digest(r.Plan) != before {
			t.Fatal("prior installer snapshot changed the applied installation or release")
		}
		var protected deploy.State
		if err := privateJSON(filepath.Join(r.Config.StateDirectory, r.Plan.ID, "inputs", "deployment-state.json"), &protected); err != nil {
			t.Fatal(err)
		}
		if protected.Bundle.ID != "r43" || protected.Policy.Release != "r43" || protected.Policy.ManagementHost != r.Plan.Installation.ManagementHost || protected.Bindings["a"].ServerID != "protected-binding" {
			t.Fatal("protected installer did not combine the applied plan with retained bindings")
		}
		var selection recovery.Config
		if err := privateJSON(r.effectiveConfig(), &selection); err != nil {
			t.Fatal(err)
		}
		if selection.Images.Binary != "/opt/ebpf-wg-mesh/isolated/r43/tools/skopeo" {
			t.Fatal("protection selected the prior release helper")
		}
	}
}
