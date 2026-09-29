//go:build linux

package builder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	buildkitdReadyPollInterval = 100 * time.Millisecond
	buildkitdStopTimeout       = 5 * time.Second
)

// buildkitdProc is a per-execution BuildKit daemon with isolated root and socket,
// joined to the execution's network namespace so daemon-side build work shares
// the sandboxed egress policy.
type buildkitdProc struct {
	cmd      *exec.Cmd
	sockPath string
	stderr   *boundedTailBuffer
	waitCh   chan error
}

// startBuildkitd starts a per-execution buildkitd joined to the namespace at
// netnsPath (no join when empty, for tests). The daemon runs with an explicit
// environment, in its own process group, with no insecure entitlements.
func startBuildkitd(ctx context.Context, netnsPath, binary, sockPath, rootDir string, env []string) (*buildkitdProc, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("per-execution buildkitd binary is required")
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir buildkitd root: %w", err)
	}
	_ = os.Remove(sockPath)
	stderr := &boundedTailBuffer{max: maxBuildFailureTailBytes}
	cmd := exec.Command(binary,
		"--addr", "unix://"+sockPath,
		"--root", rootDir,
		"--oci-worker=true",
		"--containerd-worker=false",
	)
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = rootDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = tailWriter{buf: stderr}
	if netnsPath == "" {
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start buildkitd: %w", err)
		}
	} else if err := startInNetNS(netnsPath, cmd); err != nil {
		return nil, err
	}
	proc := &buildkitdProc{cmd: cmd, sockPath: sockPath, stderr: stderr, waitCh: make(chan error, 1)}
	go func() { proc.waitCh <- cmd.Wait() }()
	// Kill promptly on cancellation; the goroutine ends with the execution ctx.
	go func() {
		<-ctx.Done()
		proc.kill()
	}()
	return proc, nil
}

// startInNetNS starts cmd with the child in the namespace at nsPath: the locked
// thread joins the target namespace, forks, and is restored before rejoining
// the pool. An unrestorable thread is leaked and the child is killed.
func startInNetNS(nsPath string, cmd *exec.Cmd) error {
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("capture host network namespace: %w", err)
	}
	defer hostNS.Close()
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("open sandbox network namespace: %w", err)
	}
	defer targetNS.Close()

	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()
	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("enter sandbox network namespace: %w", err)
	}
	startErr := cmd.Start()
	if restoreErr := netns.Set(hostNS); restoreErr != nil {
		unlockThread = false
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return fmt.Errorf("restore host network namespace: %w", restoreErr)
	}
	if startErr != nil {
		return fmt.Errorf("start buildkitd: %w", startErr)
	}
	return nil
}

// waitReady waits for the socket, early daemon exit, or ctx end. Context errors
// pass through so the executor maps timeout and cancellation.
func (p *buildkitdProc) waitReady(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			p.kill()
			return ctx.Err()
		case err := <-p.waitCh:
			return fmt.Errorf("buildkitd exited before becoming ready: %v: %s", err, strings.TrimSpace(string(p.stderr.Bytes())))
		default:
		}
		conn, err := net.DialTimeout("unix", p.sockPath, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			p.kill()
			return ctx.Err()
		case err := <-p.waitCh:
			return fmt.Errorf("buildkitd exited before becoming ready: %v: %s", err, strings.TrimSpace(string(p.stderr.Bytes())))
		case <-time.After(buildkitdReadyPollInterval):
		}
	}
}

// Stop terminates the daemon: SIGTERM to its process group, then SIGKILL after
// a grace period. A stopped daemon is success even on failure exit status.
func (p *buildkitdProc) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = unix.Kill(-p.cmd.Process.Pid, unix.SIGTERM)
	select {
	case <-p.waitCh:
		return nil
	case <-time.After(buildkitdStopTimeout):
		p.kill()
		select {
		case <-p.waitCh:
			return nil
		case <-time.After(buildkitdStopTimeout):
			return errors.New("stop buildkitd: timeout after SIGKILL")
		}
	}
}

func (p *buildkitdProc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
}

type tailWriter struct {
	buf *boundedTailBuffer
}

func (w tailWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return len(p), nil
}
