//go:build linux

package builder

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildkitdWaitReadyAgainstLiveSocket(t *testing.T) {
	t.Parallel()

	sockPath := filepath.Join(t.TempDir(), "bk.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()
	proc := &buildkitdProc{sockPath: sockPath, stderr: &boundedTailBuffer{max: 1024}, waitCh: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proc.waitReady(ctx); err != nil {
		t.Fatalf("waitReady against a live socket: %v", err)
	}
}

func TestBuildkitdWaitReadyTimeout(t *testing.T) {
	t.Parallel()

	proc := &buildkitdProc{
		sockPath: filepath.Join(t.TempDir(), "missing.sock"),
		stderr:   &boundedTailBuffer{max: 1024},
		waitCh:   make(chan error, 1),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := proc.waitReady(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("readiness wait took too long: %s", elapsed)
	}
}

func TestBuildkitdWaitReadyDaemonExits(t *testing.T) {
	t.Parallel()

	proc := &buildkitdProc{
		sockPath: filepath.Join(t.TempDir(), "missing.sock"),
		stderr:   &boundedTailBuffer{max: 1024},
		waitCh:   make(chan error, 1),
	}
	proc.stderr.Write([]byte("daemon exploded"))
	proc.waitCh <- errors.New("exit status 1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := proc.waitReady(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited before becoming ready") || !strings.Contains(err.Error(), "daemon exploded") {
		t.Fatalf("expected early-exit error with daemon output, got %v", err)
	}
}

// fakeDaemonScript writes an executable script that records its
// environment and sleeps, standing in for buildkitd without
// daemon semantics.
func fakeDaemonScript(t *testing.T, envPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-buildkitd.sh")
	script := "#!/bin/sh\nenv > " + envPath + "\nexec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartBuildkitdStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "daemon.env")
	binary := fakeDaemonScript(t, envPath)
	rootDir := filepath.Join(dir, "root")
	sockPath := filepath.Join(dir, "bk.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc, err := startBuildkitd(ctx, "", binary, sockPath, rootDir, []string{"PATH=/usr/bin:/bin", "HOME=" + rootDir})
	if err != nil {
		t.Fatalf("startBuildkitd: %v", err)
	}
	select {
	case err := <-proc.waitCh:
		t.Fatalf("daemon exited early: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	// Stop awaits the daemon's exit, so a nil return proves it died.
	if err := proc.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read daemon env: %v", err)
	}
	// The shell adds PWD itself; everything else must be exactly the
	// explicit environment (no ambient inheritance).
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		got[name] = value
	}
	if len(got) != 3 || got["PATH"] != "/usr/bin:/bin" || got["HOME"] != rootDir || got["PWD"] != rootDir {
		t.Fatalf("daemon must run with exactly the explicit environment, got %q", data)
	}
}

func TestStartBuildkitdKillsOnCancel(t *testing.T) {
	dir := t.TempDir()
	binary := fakeDaemonScript(t, filepath.Join(dir, "daemon.env"))
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := startBuildkitd(ctx, "", binary, filepath.Join(dir, "bk.sock"), filepath.Join(dir, "root"), []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("startBuildkitd: %v", err)
	}
	cancel()
	select {
	case <-proc.waitCh:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not die after context cancellation")
	}
	_ = proc.Stop()
}

func TestStartBuildkitdRejectsBadInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := startBuildkitd(ctx, "", "", filepath.Join(t.TempDir(), "x.sock"), t.TempDir(), nil); err == nil {
		t.Fatal("expected empty binary to fail")
	}
	if _, err := startBuildkitd(ctx, "/nonexistent/netns", "/bin/sleep", filepath.Join(t.TempDir(), "x.sock"), t.TempDir(), nil); err == nil {
		t.Fatal("expected missing netns to fail")
	}
	if _, err := startBuildkitd(ctx, "", "definitely-not-a-buildkitd-binary", filepath.Join(t.TempDir(), "x.sock"), t.TempDir(), nil); err == nil {
		t.Fatal("expected missing binary to fail")
	}
}

func TestBuildkitdStopNilSafe(t *testing.T) {
	t.Parallel()

	var proc *buildkitdProc
	if err := proc.Stop(); err != nil {
		t.Fatalf("nil Stop: %v", err)
	}
}
