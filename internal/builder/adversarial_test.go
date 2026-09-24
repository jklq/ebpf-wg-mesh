package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file is the shared adversarial suite for 2.4b: the same six
// hostile-build cases run against the development executor's host
// containment and the hardened executor's sandbox containment. Each
// case asserts per-backend expectations: the sandbox must contain the
// probe, while the development backend is expected to fail open in
// documented ways — the suite proves the development executor is
// honestly labeled rather than silently weaker.
//
// Cases that need no network run with denied egress so they exercise
// the loopback-only sandbox path without CNI. Only the denied-CIDR
// enforcement proof (Linux, privileged) attaches CNI.

// probeBackend runs a hostile probe through one containment backend.
type probeBackend interface {
	name() string
	// isolating reports whether the backend claims to hold hostile
	// code. Cases pick expectations from it.
	isolating() bool
	runProbe(ctx context.Context, t *testing.T, env *probeEnv, spec probeSpec) probeResult
}

// probeSpec is one hostile probe. Scripts address workspace files by
// relative path (both backends run with the workspace root as the
// working directory) and address unmounted host paths through
// absolute variables, which resolve only where no isolation exists.
type probeSpec struct {
	Script string
	// Files are fixtures written to the workspace root before the run.
	Files map[string]string
	// Env carries extra variables (identical values on both backends).
	Env     map[string]string
	Limits  ResourceLimits
	Policy  NetworkPolicy
	Timeout time.Duration
	// NprocHeadroom sizes the development backend's process limit
	// above the machine's current UID-wide thread count (RLIMIT_NPROC
	// counts every thread of the UID, so an absolute limit would
	// fork-fail on a busy machine or prove nothing on an idle one).
	// Zero means a generous default. The sandbox ignores it: cgroup
	// PIDs are per-execution either way.
	NprocHeadroom int64
	// ExtraMounts are sandbox-only mounts. The development backend
	// ignores them because its children see the whole host.
	ExtraMounts []SandboxMount
}

// defaultNprocHeadroom lets ordinary probes fork freely above the
// machine's UID-wide thread count.
const defaultNprocHeadroom = 4096

// devProcessLimits sizes the development backend's process limits for
// the machine: the process count floats above the UID-wide thread
// count, exactly as a builder operator must size RLIMIT_NPROC.
func devProcessLimits(t *testing.T, spec probeSpec) ProcessLimits {
	t.Helper()
	limits := spec.Limits
	headroom := spec.NprocHeadroom
	if headroom <= 0 {
		headroom = defaultNprocHeadroom
	}
	if baseline, err := currentUIDThreadCount(); err != nil {
		limits.MaxProcesses = 1 << 20
	} else {
		limits.MaxProcesses = baseline + headroom
	}
	return limits.ProcessLimits()
}

// probeResult is the observed probe outcome.
type probeResult struct {
	ExitCode int
	Output   string
}

// probeEnv lays out identical fixtures for both backends: a workspace
// root with a read-only snapshot, scratch, and tmp, plus a sibling
// build's files and host files outside the workspace that no sandbox
// mounts.
type probeEnv struct {
	root    string
	repo    string
	scratch string
	tmp     string
	sibling string
	outside string
}

func setupProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	base := t.TempDir()
	env := &probeEnv{
		root:    filepath.Join(base, "root"),
		repo:    filepath.Join(base, "root", "repo"),
		scratch: filepath.Join(base, "root", "scratch"),
		tmp:     filepath.Join(base, "root", "tmp"),
		sibling: filepath.Join(base, "sibling"),
		outside: filepath.Join(base, "outside"),
	}
	for _, dir := range []string{env.repo, env.scratch, env.tmp, env.sibling, env.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := map[string]string{
		filepath.Join(env.repo, "secret"):    "WORKSPACE-SECRET",
		filepath.Join(env.sibling, "secret"): "SIBLING-SECRET",
		filepath.Join(env.outside, "secret"): "OUTSIDE-SECRET",
	}
	for path, content := range fixtures {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The snapshot is read-only in both executors (chmod in the
	// development executor plus a read-only bind for root,
	// read-only bind in the sandbox), so the fixture matches either
	// way.
	if err := filepath.WalkDir(env.repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o444)
		if entry.IsDir() {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	if err := remountSnapshotReadOnly(env.repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unmountSnapshot(env.repo)
		_ = filepath.WalkDir(env.repo, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
	return env
}

func probeLimits() ResourceLimits {
	return ResourceLimits{
		Timeout: time.Minute,
		// Generous virtual headroom: one probe re-executes the Go
		// test binary, which reserves multi-gigabyte virtual arenas
		// at startup (the documented 2.4a RLIMIT_AS caveat). The
		// sandbox additionally enforces this as a resident-set cap
		// the probes never approach.
		MemoryBytes:       16 << 30,
		CPUSeconds:        60,
		MaxFileBytes:      1 << 30,
		MaxProcesses:      512,
		MaxWorkspaceBytes: 1 << 30,
	}
}

func denyEgressPolicy() NetworkPolicy {
	return NetworkPolicy{AllowGeneralEgress: false}
}

var probeIDSalt uint64

func nextProbeID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), atomic.AddUint64(&probeIDSalt, 1))
}

// devProbeBackend runs probes as host children through the same path
// the development executor uses: osCommandRunner with explicit
// environment and process limits. It is the honest baseline the
// sandbox must beat.
type devProbeBackend struct{}

func (devProbeBackend) name() string    { return "development" }
func (devProbeBackend) isolating() bool { return false }
func (devProbeBackend) runProbe(ctx context.Context, t *testing.T, env *probeEnv, spec probeSpec) probeResult {
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
	probeID := nextProbeID()
	// Probes that touch the shared /tmp namespace it per probe; the
	// host side cleans up (the sandbox side is tmpfs).
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(os.TempDir(), "probe-"+probeID)) })
	probeEnv := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + env.scratch,
		"TMPDIR=" + env.tmp,
		"PROBE_ID=" + probeID,
		"SIBLING=" + env.sibling,
		"OUTSIDE=" + env.outside,
	}
	for name, value := range spec.Env {
		probeEnv = append(probeEnv, name+"="+value)
	}
	limits := devProcessLimits(t, spec)
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := (osCommandRunner{}).Run(runCtx, commandRequest{
		Dir:    env.root,
		Binary: "/bin/sh",
		Env:    probeEnv,
		Args:   []string{"-c", spec.Script},
		Limits: &limits,
	}, nil)
	result := probeResult{Output: string(output)}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	if runCtx.Err() != nil {
		result.ExitCode = -1
		return result
	}
	t.Fatalf("probe run: %v", err)
	return result
}

func requireProbeMarker(t *testing.T, backend probeBackend, output, marker, want string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), marker+"="); ok {
			if value != want {
				t.Fatalf("%s backend: %s = %q, want %q (output:\n%s)", backend.name(), marker, value, want, output)
			}
			return
		}
	}
	t.Fatalf("%s backend: marker %s missing (output:\n%s)", backend.name(), marker, output)
}

// adversarialCases is the whole suite. TestAdversarialDevelopment
// and TestAdversarialSandbox run this identical list.
var adversarialCases = []struct {
	name string
	run  func(t *testing.T, backend probeBackend)
}{
	{"filesystem-escape", adversarialFilesystemEscape},
	{"host-socket-access", adversarialHostSocketAccess},
	{"fork-bomb", adversarialForkBomb},
	{"disk-exhaustion", adversarialDiskExhaustion},
	{"network-denial", adversarialNetworkDenial},
	{"cross-project-cache", adversarialCrossProjectCache},
}

func TestAdversarialDevelopment(t *testing.T) {
	if newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{}, "/usr/bin:/bin").Isolating() {
		t.Fatal("development executor must report itself non-isolating")
	}
	backend := devProbeBackend{}
	for _, c := range adversarialCases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, backend)
		})
	}
}

// adversarialFilesystemEscape probes reads and writes outside the
// workspace: a sibling build's files, host files outside the
// workspace, and the read-only snapshot.
func adversarialFilesystemEscape(t *testing.T, backend probeBackend) {
	t.Helper()
	env := setupProbeEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := backend.runProbe(ctx, t, env, probeSpec{
		Script: `
			echo "workspace=$(cat repo/secret 2>/dev/null || echo MISSING)"
			echo "sibling=$(cat "$SIBLING/secret" 2>/dev/null || echo MISSING)"
			echo "outside=$(cat "$OUTSIDE/secret" 2>/dev/null || echo MISSING)"
			echo probe > "$OUTSIDE/write-test" 2>/dev/null && echo "outside_write=YES" || echo "outside_write=NO"
			echo probe > repo/write-test 2>/dev/null && echo "repo_write=YES" || echo "repo_write=NO"
		`,
		Limits: probeLimits(),
		Policy: denyEgressPolicy(),
	})
	if backend.isolating() {
		requireProbeMarker(t, backend, result.Output, "workspace", "WORKSPACE-SECRET")
		requireProbeMarker(t, backend, result.Output, "sibling", "MISSING")
		requireProbeMarker(t, backend, result.Output, "outside", "MISSING")
		requireProbeMarker(t, backend, result.Output, "outside_write", "NO")
		requireProbeMarker(t, backend, result.Output, "repo_write", "NO")
		return
	}
	// The development executor documents host containment only: the
	// probe sees the sibling build, the host files, and the writable
	// host view. The snapshot stays read-only through permissions.
	requireProbeMarker(t, backend, result.Output, "workspace", "WORKSPACE-SECRET")
	requireProbeMarker(t, backend, result.Output, "sibling", "SIBLING-SECRET")
	requireProbeMarker(t, backend, result.Output, "outside", "OUTSIDE-SECRET")
	requireProbeMarker(t, backend, result.Output, "outside_write", "YES")
	requireProbeMarker(t, backend, result.Output, "repo_write", "NO")
}

// adversarialHostSocketAccess probes for host sockets. The sandbox
// must hide them all; the development probe must see exactly the
// host's own view, proving no filesystem isolation is claimed.
func adversarialHostSocketAccess(t *testing.T, backend probeBackend) {
	t.Helper()
	env := setupProbeEnv(t)
	sockets := []string{
		"/run/containerd/containerd.sock",
		"/var/run/docker.sock",
		"/run/buildkit/buildkitd.sock",
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := backend.runProbe(ctx, t, env, probeSpec{
		Script: `
			for s in /run/containerd/containerd.sock /var/run/docker.sock /run/buildkit/buildkitd.sock; do
				if [ -e "$s" ]; then echo "present=$s"; else echo "absent=$s"; fi
			done
		`,
		Limits: probeLimits(),
		Policy: denyEgressPolicy(),
	})
	if backend.isolating() {
		for _, socket := range sockets {
			if !strings.Contains(result.Output, "absent="+socket) {
				t.Fatalf("sandbox must hide %s (output:\n%s)", socket, result.Output)
			}
		}
		return
	}
	for _, socket := range sockets {
		_, statErr := os.Stat(socket)
		want := "absent=" + socket
		if statErr == nil {
			want = "present=" + socket
		}
		if !strings.Contains(result.Output, want) {
			t.Fatalf("development probe must see the host view of %s (output:\n%s)", socket, result.Output)
		}
	}
}

// adversarialForkBomb runs two concurrent capped process spawns. The
// sandbox gives each execution its own PID budget (both probes spawn
// fully); the development backend shares the UID-wide rlimit budget,
// so the two probes cannot both spawn fully — the pigeonhole
// principle, not machine load, guarantees it.
func adversarialForkBomb(t *testing.T, backend probeBackend) {
	t.Helper()
	const spawns = 300
	// The loop and the spawn count use shell builtins only (while,
	// read, set): under an exhausted fork budget even seq, cat, and
	// wc cannot start, which would hide the count the case asserts
	// on. A probe that cannot fork at all aborts before printing;
	// the counter below reads that as zero.
	script := `
		i=0
		while [ "$i" -lt 300 ]; do i=$((i+1)); sleep 5 & done
		sleep 1
		kids=""
		read kids < /proc/$$/task/$$/children 2>/dev/null || true
		set -- $kids
		echo "spawned=$#"
		wait
	`
	limits := probeLimits()
	limits.MaxProcesses = 512
	// The two concurrent probes spawn 600 processes against 128
	// above the UID baseline: the shared budget cannot fit both, so
	// at least one development probe is capped no matter how busy or
	// idle the machine is. Each sandbox holds its own 512 budget, so
	// both sandboxed probes spawn fully.
	run := func(env *probeEnv) probeResult {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return backend.runProbe(ctx, t, env, probeSpec{Script: script, Limits: limits, Policy: denyEgressPolicy(), Timeout: time.Minute, NprocHeadroom: 128})
	}
	envFirst, envSecond := setupProbeEnv(t), setupProbeEnv(t)
	firstCh := make(chan probeResult, 1)
	go func() { firstCh <- run(envFirst) }()
	second := run(envSecond)
	first := <-firstCh
	// count parses the spawn count. A shell whose fork budget runs
	// out aborts before printing; that is a capped spawn, reported
	// as aborted rather than a literal zero.
	count := func(result probeResult) (int, bool) {
		for _, line := range strings.Split(result.Output, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "spawned="); ok {
				var n int
				if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
					return n, false
				}
			}
		}
		if strings.Contains(result.Output, "Cannot fork") {
			return 0, true
		}
		t.Fatalf("%s backend: spawned marker missing (output:\n%s)", backend.name(), result.Output)
		return 0, false
	}
	firstN, firstAborted := count(first)
	secondN, secondAborted := count(second)
	t.Logf("%s backend: spawned %d (aborted=%v) and %d (aborted=%v) of %d",
		backend.name(), firstN, firstAborted, secondN, secondAborted, spawns)
	if backend.isolating() {
		if firstAborted || secondAborted || firstN < spawns-5 || secondN < spawns-5 {
			t.Fatalf("sandboxed probes must each spawn ~%d, got %d and %d", spawns, firstN, secondN)
		}
		return
	}
	if os.Geteuid() == 0 {
		// Any uid 0 (real root or a user namespace) bypasses
		// RLIMIT_NPROC — verified: a sub-baseline nproc limit still
		// spawns freely — so as root the development backend has no
		// fork budget at all, shared or otherwise. Both probes must
		// spawn fully, which is the honest statement of that.
		if firstAborted || secondAborted || firstN < spawns-5 || secondN < spawns-5 {
			t.Fatalf("root development probes bypass the process budget: expected ~%d each, got %d and %d", spawns, firstN, secondN)
		}
		return
	}
	firstCapped := firstAborted || firstN < spawns
	secondCapped := secondAborted || secondN < spawns
	if !firstCapped && !secondCapped {
		t.Fatalf("development probes share the UID-wide process budget: both spawned %d, expected at least one capped", spawns)
	}
}

// adversarialDiskExhaustion fills the workspace temp dir and /tmp far
// past the disk limit. Bind-mounted writes are detected post-hoc in
// both backends; only the sandbox also prevents /tmp writes via its
// sized tmpfs.
func adversarialDiskExhaustion(t *testing.T, backend probeBackend) {
	t.Helper()
	env := setupProbeEnv(t)
	limits := probeLimits()
	limits.MaxWorkspaceBytes = 32 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := backend.runProbe(ctx, t, env, probeSpec{
		Script: `
			mkdir -p "/tmp/probe-$PROBE_ID"
			dd if=/dev/zero of="$TMPDIR/fill" bs=1048576 count=256 2>/dev/null
			echo "tmp_wrote=$(wc -c < "$TMPDIR/fill" 2>/dev/null | tr -d ' ' || echo 0)"
			dd if=/dev/zero of="/tmp/probe-$PROBE_ID/fill" bs=1048576 count=256 2>/dev/null
			echo "slash_tmp_wrote=$(wc -c < "/tmp/probe-$PROBE_ID/fill" 2>/dev/null | tr -d ' ' || echo 0)"
			[ "$slash_tmp_wrote" != "" ]
		`,
		Limits:  limits,
		Policy:  denyEgressPolicy(),
		Timeout: 2 * time.Minute,
	})
	bytesOf := func(marker string) int64 {
		for _, line := range strings.Split(result.Output, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), marker+"="); ok {
				var n int64
				if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
					return n
				}
			}
		}
		t.Fatalf("%s backend: marker %s missing (output:\n%s)", backend.name(), marker, result.Output)
		return 0
	}
	const want = int64(256 << 20)
	if backend.isolating() {
		// The workspace bind fill is detected post-hoc like the
		// development backend; the /tmp fill is prevented by the
		// 16m tmpfs derived from the 32m disk budget.
		if got := bytesOf("slash_tmp_wrote"); got >= 32<<20 {
			t.Fatalf("sandbox /tmp fill wrote %d bytes, exceeding the tmpfs cap (output:\n%s)", got, result.Output)
		}
	} else {
		if got := bytesOf("tmp_wrote"); got != want {
			t.Fatalf("development TMPDIR fill wrote %d, want %d (output:\n%s)", got, want, result.Output)
		}
		if got := bytesOf("slash_tmp_wrote"); got != want {
			t.Fatalf("development /tmp fill wrote %d, want %d (output:\n%s)", got, want, result.Output)
		}
	}
	// Both executors account the workspace afterwards: the 256m bind
	// fill must fail the 32m budget.
	if err := enforceWorkspaceDiskLimit(env.root, limits.MaxWorkspaceBytes); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("%s backend: expected post-hoc disk accounting to fail, got %v", backend.name(), err)
	}
}

// startLoopbackListener listens on host loopback and reports the
// first received payload.
func startLoopbackListener(t *testing.T) (port string, received chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, port, err = net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	received = make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		data, _ := io.ReadAll(conn)
		received <- string(data)
	}()
	return port, received
}

// TestProbeDialHelper is a test-binary helper, not a test: the
// development loopback probe re-executes the test binary with
// GO_ADVERSARIAL_DIAL set to dial host loopback through the same
// host-child path the development executor uses.
func TestProbeDialHelper(t *testing.T) {
	addr := os.Getenv("GO_ADVERSARIAL_DIAL")
	if addr == "" {
		return
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(conn, "probe-token"); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// adversarialNetworkDenial checks whether the probe reaches a host
// loopback listener. The sandbox's private loopback must not reach
// it; the development child shares the host network and must.
func adversarialNetworkDenial(t *testing.T, backend probeBackend) {
	t.Helper()
	env := setupProbeEnv(t)
	port, received := startLoopbackListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if backend.isolating() {
		result := backend.runProbe(ctx, t, env, probeSpec{
			Script:  "echo probe-token | nc -w 5 127.0.0.1 " + port,
			Limits:  probeLimits(),
			Policy:  denyEgressPolicy(),
			Timeout: time.Minute,
		})
		if result.ExitCode == 0 {
			t.Fatalf("sandbox reached host loopback (output:\n%s)", result.Output)
		}
		select {
		case payload := <-received:
			t.Fatalf("host listener received %q from the sandbox", payload)
		case <-time.After(3 * time.Second):
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	limits := devProcessLimits(t, probeSpec{Limits: probeLimits()})
	runCtx, runCancel := context.WithTimeout(ctx, time.Minute)
	defer runCancel()
	output, err := (osCommandRunner{}).Run(runCtx, commandRequest{
		Dir:    env.root,
		Binary: executable,
		Env: []string{
			"PATH=/usr/bin:/bin",
			"HOME=" + env.scratch,
			"TMPDIR=" + env.tmp,
			"GO_ADVERSARIAL_DIAL=127.0.0.1:" + port,
		},
		Args:   []string{"-test.run=TestProbeDialHelper"},
		Limits: &limits,
	}, nil)
	if err != nil {
		t.Fatalf("development child must reach host loopback: %v (output %q)", err, output)
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, "probe-token") {
			t.Fatalf("unexpected loopback payload %q", payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host listener received nothing from the development child")
	}
}

// adversarialCrossProjectCache gives the probe two content-keyed
// cache dirs but mounts only its own. The sandbox must hide the
// sibling key; the development child sees the whole host.
func adversarialCrossProjectCache(t *testing.T, backend probeBackend) {
	t.Helper()
	env := setupProbeEnv(t)
	base := filepath.Dir(env.root)
	keyA := filepath.Join(base, "cache", "cache-sha256-aaaa")
	keyB := filepath.Join(base, "cache", "cache-sha256-bbbb")
	for dir, secret := range map[string]string{keyA: "SECRET-A", keyB: "SECRET-B"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "secret"), []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := probeSpec{
		Script: `
			echo "a=$(cat "$CACHE_A/secret" 2>/dev/null || echo MISSING)"
			echo "b=$(cat "$CACHE_B/secret" 2>/dev/null || echo MISSING)"
		`,
		Limits: probeLimits(),
		Policy: denyEgressPolicy(),
	}
	if backend.isolating() {
		spec.Env = map[string]string{"CACHE_A": keyA, "CACHE_B": "/build/cache"}
		spec.ExtraMounts = []SandboxMount{{Source: keyB, Dest: "/build/cache"}}
		result := backend.runProbe(ctx, t, env, spec)
		requireProbeMarker(t, backend, result.Output, "a", "MISSING")
		requireProbeMarker(t, backend, result.Output, "b", "SECRET-B")
		return
	}
	spec.Env = map[string]string{"CACHE_A": keyA, "CACHE_B": keyB}
	result := backend.runProbe(ctx, t, env, spec)
	requireProbeMarker(t, backend, result.Output, "a", "SECRET-A")
	requireProbeMarker(t, backend, result.Output, "b", "SECRET-B")
}
