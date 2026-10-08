//go:build linux && integration

package builder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestNativeProtectedBuilderToolchainImportAndRuntime(t *testing.T) {
	archive, ref := os.Getenv("BUILDER_RUNTIME_ARCHIVE"), os.Getenv("BUILDER_RUNTIME_REFERENCE")
	if archive == "" || ref == "" {
		t.Skip("requires an isolated root containerd host and protected OCI toolchain")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native containment verification requires root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := config.BuilderConfig{ID: "native-toolchain", WorkDir: t.TempDir(), BuildctlBinary: "buildctl", RailpackBinary: "railpack", Sandbox: config.BuilderSandboxConfig{Socket: "/run/containerd/containerd.sock", Namespace: "production-toolchain-test", Image: ref, Runtime: "io.containerd.runc.v2", Snapshotter: "overlayfs", CNIPluginDir: "/usr/lib/cni", CNIConfDir: "/etc/cni/net.d", CNINetwork: "mesh-cni", BuildkitdBinary: os.Getenv("BUILDKITD_BINARY")}}
	if err := VerifyRuntimeImage(ctx, cfg, ref); err == nil {
		t.Fatal("missing runtime image accepted")
	}
	if err := ImportRuntimeImage(ctx, cfg, archive, ref); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeImage(ctx, cfg, ref); err != nil {
		t.Fatal(err)
	}
	// Native imports and unpack are idempotent across interruption/retry.
	if err := ImportRuntimeImage(ctx, cfg, archive, ref); err != nil {
		t.Fatal("retry", err)
	}
	if err := VerifyRuntimeImage(ctx, cfg, strings.Split(ref, "@")[0]+"@sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("unselected image was accepted")
	}
	if err := InspectRuntime(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	nets, err := os.ReadDir(filepath.Join(cfg.WorkDir, sandboxNetnsDirName))
	if err != nil || len(nets) != 0 {
		t.Fatal("runtime inspection leaked a network", nets, err)
	}
}

func TestNativeRecoveredRailpackFrontendBuildsWithoutPublisher(t *testing.T) {
	frontend, digest := os.Getenv("RAILPACK_FRONTEND_DIRECTORY"), os.Getenv("RAILPACK_FRONTEND_DIGEST")
	if frontend == "" || digest == "" {
		t.Skip("requires recovered frontend OCI layout and native BuildKit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	daemon, err := startBuildkitd(ctx, "", os.Getenv("BUILDKITD_BINARY"), root+"/buildkit.sock", root+"/state", []string{"PATH=/native/platform-buildkit/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + root})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Stop()
	if err := daemon.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	source := root + "/source"
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	// A valid local-only Railpack deploy produces actual content without fetching
	// an application base or the frontend from any publisher/registry.
	if err := os.WriteFile(source+"/railpack-plan.json", []byte(`{"deploy":{"base":{"local":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"/proof.txt", []byte("recovered frontend executed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := root + "/output"
	command := exec.CommandContext(ctx, "/native/platform-buildkit/bin/buildctl", "--addr", "unix://"+root+"/buildkit.sock", "build", "--frontend", "gateway.v0", "--opt", "source=platform-frontend", "--opt", "context:platform-frontend=oci-layout:frontend@"+digest, "--oci-layout", "frontend="+frontend, "--local", "context="+source, "--local", "dockerfile="+source, "--opt", "filename=railpack-plan.json", "--output", "type=local,dest="+output)
	b, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native recovered frontend: %v\n%s\n%s", err, b, daemon.stderr.Bytes())
	}
	got, err := os.ReadFile(output + "/proof.txt")
	if err != nil || string(got) != "recovered frontend executed\n" {
		t.Fatal("frontend produced no actual deployment files", err, string(got))
	}
}
