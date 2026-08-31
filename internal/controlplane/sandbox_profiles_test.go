package controlplane

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/protobuf/encoding/protojson"
)

func TestResolveServiceSandboxProfileUsesOperatorDefinition(t *testing.T) {
	profiles := sandboxProfilesFromConfig(config.SandboxConfig{CompatibilityProfiles: []config.SandboxProfileConfig{{
		Name: "legacy-root", Risk: "Runs the workload as root.", Relaxations: []string{"run-as-root"},
	}}})
	spec := directImageServiceSpec("example.test/image:1", &platformv1.ServiceRuntime{
		CpuMillis: 250, MemoryMebibytes: 256,
		SandboxProfile: &platformv1.SandboxProfile{
			Name:        "legacy-root",
			Risk:        "client attempted replacement",
			Relaxations: []platformv1.SandboxRelaxation{platformv1.SandboxRelaxation_SANDBOX_RELAXATION_WRITABLE_ROOT_FILESYSTEM},
		},
	})
	if err := resolveServiceSandboxProfile(spec, profiles); err != nil {
		t.Fatal(err)
	}
	resolved := spec.GetRuntime().GetSandboxProfile()
	if resolved.GetRisk() != "Runs the workload as root." || len(resolved.GetRelaxations()) != 1 || resolved.GetRelaxations()[0] != platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT {
		t.Fatalf("client fields were not replaced by operator definition: %+v", resolved)
	}
}

func TestResolveServiceSandboxProfileRejectsUnknownName(t *testing.T) {
	spec := directImageServiceSpec("example.test/image:1", &platformv1.ServiceRuntime{
		SandboxProfile: &platformv1.SandboxProfile{Name: "privileged"},
	})
	if err := resolveServiceSandboxProfile(spec, sandboxProfilesFromConfig(config.SandboxConfig{})); err == nil {
		t.Fatal("unknown sandbox profile was accepted")
	}
}

func TestCanonicalServiceSpecDefaultsProductionSandbox(t *testing.T) {
	spec := canonicalServiceSpec(directImageServiceSpec("example.test/image:1", &platformv1.ServiceRuntime{}))
	profile := spec.GetRuntime().GetSandboxProfile()
	if profile.GetName() != productionSandboxProfileName || len(profile.GetRelaxations()) != 0 {
		t.Fatalf("default sandbox = %+v", profile)
	}
}

func TestServiceSpecJSONRoundTripKeepsCompatibilitySandbox(t *testing.T) {
	spec := canonicalServiceSpec(directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		SandboxProfile: &platformv1.SandboxProfile{
			Name: "legacy-root",
			Risk: "The image runs as root.",
			Relaxations: []platformv1.SandboxRelaxation{
				platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT,
			},
		},
	}))
	raw, err := protojson.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadServiceSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.GetRuntime().GetSandboxProfile()
	if got.GetName() != "legacy-root" || got.GetRisk() != "The image runs as root." || len(got.GetRelaxations()) != 1 {
		t.Fatalf("round-tripped sandbox = %+v from %s", got, raw)
	}
}

func TestLoadServiceSpecReadsCockroachSandboxJSON(t *testing.T) {
	raw := []byte(`{"desiredReplicaCount": 1, "runtime": {"restart": {"backoffMultiplier": 2, "initialDelayMs": 1000, "jitter": 0.1, "maxDelayMs": 60000, "maxRestarts": 5, "policy": "RESTART_POLICY_ON_FAILURE", "stableAfterSeconds": 60, "windowSeconds": 300}, "sandboxProfile": {"name": "legacy-root", "relaxations": ["SANDBOX_RELAXATION_RUN_AS_ROOT"], "risk": "The image runs as root."}}, "source": {"image": {"image": "busybox:1.36"}}}`)
	loaded, err := loadServiceSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.GetRuntime().GetSandboxProfile().GetName(); got != "legacy-root" {
		t.Fatalf("sandbox name = %q profile=%v", got, loaded.GetRuntime().GetSandboxProfile())
	}
}

func TestListedSandboxProfilesPutsProductionFirst(t *testing.T) {
	profiles := sandboxProfilesFromConfig(config.SandboxConfig{CompatibilityProfiles: []config.SandboxProfileConfig{
		{Name: "legacy-root", Risk: "root", Relaxations: []string{"run-as-root"}},
		{Name: "legacy-rw", Risk: "writable", Relaxations: []string{"writable-rootfs"}},
	}})
	listed := listedSandboxProfiles(profiles)
	if len(listed) != 3 || listed[0].GetName() != productionSandboxProfileName || listed[1].GetName() != "legacy-root" || listed[2].GetName() != "legacy-rw" {
		t.Fatalf("listed = %+v", listed)
	}
}
