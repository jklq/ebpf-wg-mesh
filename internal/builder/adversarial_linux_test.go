//go:build linux

package builder

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
)

const sandboxProbeImage = "docker.io/library/busybox:1.36.1"

// sandboxProbeBackend runs adversarial probes through the real
// containerd sandbox backend: private namespaces, zero capabilities,
// cgroup limits, and enforced egress policy.
type sandboxProbeBackend struct {
	t       *testing.T
	backend SandboxBackend
	workDir string
}

func (b *sandboxProbeBackend) name() string    { return "hardened-sandbox" }
func (b *sandboxProbeBackend) isolating() bool { return true }

var sandboxProbeSalt uint64

func (b *sandboxProbeBackend) runProbe(ctx context.Context, t *testing.T, env *probeEnv, spec probeSpec) probeResult {
	t.Helper()
	for rel, content := range spec.Files {
		target, err := safeChildPath(env.root, rel)
		if err != nil {
			t.Fatalf("probe fixture path: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	buildID := fmt.Sprintf("probe-%d-%d", os.Getpid(), atomic.AddUint64(&sandboxProbeSalt, 1))
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sandboxNet, err := b.backend.SetupNet(runCtx, buildID, spec.Policy)
	if err != nil {
		t.Fatalf("probe SetupNet: %v", err)
	}
	defer func() {
		teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer teardownCancel()
		if err := b.backend.TeardownNet(teardownCtx, sandboxNet); err != nil {
			t.Errorf("probe TeardownNet: %v", err)
		}
	}()
	probeEnv := []string{
		"PATH=" + sandboxPathEnv,
		"HOME=/build/scratch",
		"TMPDIR=/build/tmp",
		"PROBE_ID=" + buildID,
		"SIBLING=" + env.sibling,
		"OUTSIDE=" + env.outside,
	}
	for name, value := range spec.Env {
		probeEnv = append(probeEnv, name+"="+value)
	}
	mounts := []SandboxMount{
		{Source: env.root, Dest: sandboxBuildRoot},
		{Source: env.repo, Dest: sandboxBuildRoot + "/repo", ReadOnly: true},
	}
	mounts = append(mounts, spec.ExtraMounts...)
	var lines []string
	err = b.backend.RunStep(runCtx, sandboxNet, SandboxStep{
		Name:   "probe",
		Argv:   []string{"sh", "-c", spec.Script},
		Env:    probeEnv,
		Dir:    sandboxBuildRoot,
		Limits: spec.Limits,
		Mounts: mounts,
		OnLog:  func(line commandOutputLine) { lines = append(lines, line.Line) },
	})
	result := probeResult{Output: strings.Join(lines, "\n")}
	if err == nil {
		return result
	}
	if stepErr, ok := err.(*SandboxStepError); ok {
		result.ExitCode = stepErr.ExitCode
		if result.Output == "" {
			result.Output = stepErr.Tail
		} else if stepErr.Tail != "" {
			result.Output += "\n" + stepErr.Tail
		}
		return result
	}
	if runCtx.Err() != nil {
		result.ExitCode = -1
		return result
	}
	t.Fatalf("probe RunStep: %v", err)
	return result
}

// requireSandboxProbeBackend opens a real sandbox backend or skips.
// It needs root, a reachable containerd, and the probe image.
func requireSandboxProbeBackend(t *testing.T) *sandboxProbeBackend {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("sandbox probes need root for namespaces and containerd")
	}
	socket := strings.TrimSpace(os.Getenv("CONTAINERD_ADDRESS"))
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	probe, err := containerd.New(socket)
	if err != nil {
		t.Skipf("containerd is not reachable on %s: %v", socket, err)
	}
	_ = probe.Close()
	namespace := fmt.Sprintf("builder-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	ensureSandboxProbeImage(t, socket, namespace)
	workDir := t.TempDir()
	backend, err := NewSandboxBackend(SandboxBackendConfig{
		Socket:       socket,
		Namespace:    namespace,
		Image:        sandboxProbeImage,
		Runtime:      "io.containerd.runc.v2",
		Snapshotter:  "overlayfs",
		CNIPluginDir: "/usr/lib/cni",
		CNIConfDir:   t.TempDir(),
		CNINetwork:   "build-sandbox-test",
		WorkDir:      workDir,
	})
	if err != nil {
		t.Fatalf("NewSandboxBackend: %v", err)
	}
	t.Cleanup(func() {
		if _, err := backend.ReapStale(context.Background()); err != nil {
			t.Errorf("probe backend ReapStale: %v", err)
		}
		if err := backend.Close(); err != nil {
			t.Errorf("probe backend Close: %v", err)
		}
		// No sandbox container or namespace may survive its test.
		leftover := sandboxProbeLeftovers(t, socket, backend.(*containerdSandboxBackend).cfg.Namespace)
		if leftover != "" {
			t.Errorf("leftover sandbox state: %s", leftover)
		}
	})
	return &sandboxProbeBackend{t: t, backend: backend, workDir: workDir}
}

// ensureSandboxProbeImage makes the probe image available in the
// test's containerd namespace, pulling when it is missing. Image
// metadata is namespaced (blobs are content-shared, so the pull is
// cheap when CI already pulled into another namespace); a missing
// image without a working pull skips instead of failing.
func ensureSandboxProbeImage(t *testing.T, socket, namespace string) {
	t.Helper()
	client, err := containerd.New(socket)
	if err != nil {
		t.Skipf("containerd is not reachable on %s: %v", socket, err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), namespace), 5*time.Minute)
	defer cancel()
	if _, err := client.GetImage(ctx, sandboxProbeImage); err == nil {
		return
	}
	if _, err := client.Pull(ctx, sandboxProbeImage, containerd.WithPullUnpack, containerd.WithPullSnapshotter("overlayfs")); err != nil {
		t.Skipf("sandbox probe image %s is not available: %v", sandboxProbeImage, err)
	}
}

func sandboxProbeLeftovers(t *testing.T, socket, namespace string) string {
	t.Helper()
	client, err := containerd.New(socket)
	if err != nil {
		return ""
	}
	defer client.Close()
	ctx := namespaces.WithNamespace(context.Background(), namespace)
	records, err := client.ContainerService().List(ctx)
	if err != nil {
		return ""
	}
	var ids []string
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return strings.Join(ids, ",")
}

func TestAdversarialSandbox(t *testing.T) {
	backend := requireSandboxProbeBackend(t)
	for _, c := range adversarialCases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, backend)
		})
	}
}

// TestSandboxTrueForkBombContained detonates the classic fork bomb
// inside a sandbox with a tight PID budget. The sandbox must survive
// and a neighbor probe must be unaffected. There is deliberately no
// development-backend counterpart: detonating a fork bomb as a host
// child would spend the whole machine's UID-wide fork budget, while
// the capped-spawn case already proves the development backend lacks
// per-execution PID containment.
func TestSandboxTrueForkBombContained(t *testing.T) {
	backend := requireSandboxProbeBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	victim := backend.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script:  `echo "victim=$(cat repo/secret)"`,
		Limits:  probeLimits(),
		Policy:  denyEgressPolicy(),
		Timeout: time.Minute,
	})
	requireProbeMarker(t, backend, victim.Output, "victim", "WORKSPACE-SECRET")

	limits := probeLimits()
	limits.MaxProcesses = 64
	// The wait after detonation is a builtin-only spin: once the
	// bomb fills the PID budget, even sleep cannot fork, so any
	// external wait would abort the shell before the marker. The
	// fork-failure noise in the output proves the bomb actually
	// raged against the cap instead of fizzling on shell syntax.
	bomb := backend.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script: `
			:(){ :|:& };:
			i=0
			while [ "$i" -lt 500000 ]; do i=$((i+1)); done
			echo "survived=yes"
		`,
		Limits:  limits,
		Policy:  denyEgressPolicy(),
		Timeout: time.Minute,
	})
	requireProbeMarker(t, backend, bomb.Output, "survived", "yes")
	if !strings.Contains(bomb.Output, "fork") {
		t.Fatalf("expected fork-cap evidence in bomb output (output:\n%s)", bomb.Output)
	}

	after := backend.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script:  `echo "victim=$(cat repo/secret)"`,
		Limits:  probeLimits(),
		Policy:  denyEgressPolicy(),
		Timeout: time.Minute,
	})
	requireProbeMarker(t, backend, after.Output, "victim", "WORKSPACE-SECRET")
}

// cniPluginDirForTest locates the CNI bridge plugin, skipping when it
// is not installed.
func cniPluginDirForTest(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/usr/lib/cni", "/opt/cni/bin"} {
		if _, err := os.Stat(filepath.Join(dir, "bridge")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "host-local")); err == nil {
				return dir
			}
		}
	}
	t.Skip("CNI bridge/host-local plugins are not installed")
	return ""
}

// TestSandboxDeniedCIDREnforced proves data-plane egress enforcement:
// with no denies the sandbox reaches the CNI gateway; with the
// gateway's own /32 denied, the same address is unreachable. The
// gateway is on-link either way, so only the blackhole explains the
// difference.
func TestSandboxDeniedCIDREnforced(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("sandbox CNI test needs root")
	}
	socket := strings.TrimSpace(os.Getenv("CONTAINERD_ADDRESS"))
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	if probe, err := containerd.New(socket); err != nil {
		t.Skipf("containerd is not reachable on %s: %v", socket, err)
	} else {
		_ = probe.Close()
	}
	pluginDir := cniPluginDirForTest(t)
	namespace := fmt.Sprintf("builder-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	ensureSandboxProbeImage(t, socket, namespace)
	confDir := t.TempDir()
	conflist := `{
	  "cniVersion": "1.0.0",
	  "name": "build-sandbox-test",
	  "plugins": [
	    {"type": "bridge", "bridge": "bld0", "isGateway": true, "ipMasq": false,
	     "ipam": {"type": "host-local", "ranges": [[{"subnet": "10.233.0.0/24"}]],
	              "routes": [{"dst": "0.0.0.0/0"}]}},
	    {"type": "loopback"}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(confDir, "build-sandbox-test.conflist"), []byte(conflist), 0o644); err != nil {
		t.Fatal(err)
	}
	backend, err := NewSandboxBackend(SandboxBackendConfig{
		Socket:       socket,
		Namespace:    namespace,
		Image:        sandboxProbeImage,
		Runtime:      "io.containerd.runc.v2",
		Snapshotter:  "overlayfs",
		CNIPluginDir: pluginDir,
		CNIConfDir:   confDir,
		CNINetwork:   "build-sandbox-test",
		WorkDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewSandboxBackend: %v", err)
	}
	defer func() {
		_, _ = backend.ReapStale(context.Background())
		_ = backend.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	probe := &sandboxProbeBackend{t: t, backend: backend}

	// The gateway address exists once the first CNI attach creates
	// the bridge. Bind the control listener then and keep it for
	// both probes.
	firstNet, err := backend.SetupNet(ctx, "gateway-control", NetworkPolicy{AllowGeneralEgress: true})
	if err != nil {
		t.Fatalf("control SetupNet: %v", err)
	}
	listener, err := net.Listen("tcp", "10.233.0.1:0")
	if err != nil {
		_ = backend.TeardownNet(context.Background(), firstNet)
		t.Fatalf("listen on CNI gateway: %v", err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split gateway addr: %v", err)
	}
	received := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				data := make([]byte, 64)
				n, _ := conn.Read(data)
				received <- string(data[:n])
			}()
		}
	}()

	control := probe.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script: fmt.Sprintf(`echo gateway-token | nc -w 5 10.233.0.1 %s && echo "reached=yes" || echo "reached=no"`, port),
		Limits: probeLimits(),
		Policy: NetworkPolicy{AllowGeneralEgress: true},
	})
	requireProbeMarker(t, probe, control.Output, "reached", "yes")
	select {
	case payload := <-received:
		if !strings.Contains(payload, "gateway-token") {
			t.Fatalf("unexpected gateway payload %q", payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gateway listener received nothing from the control probe")
	}
	_ = backend.TeardownNet(context.Background(), firstNet)

	denied := probe.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script: fmt.Sprintf(`echo gateway-token | nc -w 5 10.233.0.1 %s && echo "reached=yes" || echo "reached=no"`, port),
		Limits: probeLimits(),
		Policy: NetworkPolicy{AllowGeneralEgress: true, DeniedCIDRs: []string{"10.233.0.1/32"}},
	})
	requireProbeMarker(t, probe, denied.Output, "reached", "no")
	select {
	case payload := <-received:
		t.Fatalf("gateway listener received %q despite the /32 deny", payload)
	case <-time.After(3 * time.Second):
	}

	// The platform metadata addresses stay unreachable too.
	metadata := probe.runProbe(ctx, t, setupProbeEnv(t), probeSpec{
		Script: `echo x | nc -w 3 169.254.169.254 80 && echo "reached=yes" || echo "reached=no"`,
		Limits: probeLimits(),
		Policy: DefaultRestrictedNetworkPolicy(),
	})
	requireProbeMarker(t, probe, metadata.Output, "reached", "no")
}
