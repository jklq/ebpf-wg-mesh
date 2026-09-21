//go:build linux

package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// limitHelperRequest builds a helper invocation carrying extra
// environment alongside the standard helper variables.
func limitHelperRequest(mode string, limits *ProcessLimits, extraEnv ...string) commandRequest {
	req := helperCommandRequest(mode)
	req.Limits = limits
	req.Env = append(req.Env, "PATH=/usr/bin:/bin")
	req.Env = append(req.Env, extraEnv...)
	return req
}

func TestOSCommandRunnerEnforcesGenerousLimits(t *testing.T) {
	t.Parallel()

	// RLIMIT_NPROC counts every thread of the builder UID, not just the
	// build child, so this must clear ambient per-user thread usage on
	// the test machine (development boxes often run near a thousand).
	limits := &ProcessLimits{
		MemoryBytes:  4 << 30,
		CPUSeconds:   60,
		MaxFileBytes: 64 << 20,
		MaxProcesses: 16384,
	}
	if _, err := (osCommandRunner{}).Run(context.Background(), limitHelperRequest("streams", limits), nil); err != nil {
		t.Fatalf("generous limits must not break a normal build child: %v", err)
	}
}

func TestOSCommandRunnerEnforcesCPULimit(t *testing.T) {
	t.Parallel()

	limits := &ProcessLimits{CPUSeconds: 1}
	start := time.Now()
	err := runHelperForTest("spin", limits)
	if err == nil {
		t.Fatal("expected CPU-bound child to be killed")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("CPU limit took too long to fire: %s", elapsed)
	}
}

func TestOSCommandRunnerEnforcesMemoryLimit(t *testing.T) {
	t.Parallel()

	// Go build children reserve ~1.2 GiB of virtual address space at
	// startup, so the limit must clear that before the allocation
	// under test can trip it.
	limits := &ProcessLimits{MemoryBytes: 2 << 30}
	control := limitHelperRequest("streams", limits)
	if _, err := (osCommandRunner{}).Run(context.Background(), control, nil); err != nil {
		t.Fatalf("child must start under the memory limit: %v", err)
	}
	req := limitHelperRequest("alloc", limits, "HELPER_MB=2048")
	if _, err := (osCommandRunner{}).Run(context.Background(), req, nil); err == nil {
		t.Fatal("expected over-allocation child to fail")
	}
}

func TestOSCommandRunnerEnforcesFileSizeLimit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bulk.dat")
	limits := &ProcessLimits{MaxFileBytes: 4096}
	req := limitHelperRequest("bigfile", limits,
		"HELPER_BYTES="+strconv.Itoa(1<<20),
		"HELPER_FILE="+path,
	)
	if _, err := (osCommandRunner{}).Run(context.Background(), req, nil); err == nil {
		t.Fatal("expected oversized file write to fail")
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 4096 {
		t.Fatalf("file grew past the limit: %d bytes", info.Size())
	}
}

func TestOSCommandRunnerEnforcesProcessLimit(t *testing.T) {
	t.Parallel()

	// Any uid 0 bypasses RLIMIT_NPROC (verified for real root and
	// user namespaces alike), so the capped expectation only holds
	// unprivileged. The privileged suite covers PID containment at
	// the sandbox instead.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses RLIMIT_NPROC: the limit under test cannot bind here")
	}

	limits := &ProcessLimits{MaxProcesses: 64}
	req := limitHelperRequest("fork", limits, "HELPER_CHILDREN=128")
	if _, err := (osCommandRunner{}).Run(context.Background(), req, nil); err == nil {
		t.Fatal("expected fork-heavy child to fail")
	}

	control := limitHelperRequest("fork", nil, "HELPER_CHILDREN=16")
	if _, err := (osCommandRunner{}).Run(context.Background(), control, nil); err != nil {
		t.Fatalf("modest fan-out without limits must succeed: %v", err)
	}
}

func TestOSCommandRunnerCancellationKillsProcessTree(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	req := limitHelperRequest("spawn", nil, "HELPER_PIDFILE="+pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (osCommandRunner{}).Run(ctx, req, nil)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	var grandchild int
	for grandchild == 0 && time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			grandchild, _ = strconv.Atoi(string(data))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		cancel()
		t.Fatal("timed out waiting for grandchild to start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for cancelled child")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(grandchild, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived cancellation", grandchild)
}

func runHelperForTest(mode string, limits *ProcessLimits) error {
	_, err := (osCommandRunner{}).Run(context.Background(), limitHelperRequest(mode, limits), nil)
	return err
}
