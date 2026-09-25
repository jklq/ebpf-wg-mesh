package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/testutil"
)

type consoleProcess struct {
	cmd     *exec.Cmd
	done    <-chan struct{}
	waitErr error
}

func startConsole(ctx context.Context, consoleDir string, env map[string]string, port int, bindAddress string, reserved net.Listener) (*consoleProcess, error) {
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

	// The console's generated protobuf client is not checked in; regenerate it
	// so the console never runs against stale RPC types.
	generate, err := localteststack.ChildCommand(ctx, "bun", []string{"run", "generate:proto"}, cloneEnvironmentOverrides(env))
	if err != nil {
		return nil, err
	}
	generate.Dir = consoleDir
	generate.Stdout = os.Stdout
	generate.Stderr = os.Stderr
	if err := generate.Run(); err != nil {
		if reserved != nil {
			_ = reserved.Close()
		}
		return nil, fmt.Errorf("generate console protobuf client: %w", err)
	}

	var cmd *exec.Cmd
	if os.Getenv("LOCALTESTSTACK_CONSOLE_PRODUCTION") == "1" {
		buildEnv := cloneEnvironmentOverrides(env)
		buildEnv["NITRO_PRESET"] = "bun"
		build, err := localteststack.ChildCommand(ctx, "bun", []string{"--bun", "vite", "build"}, buildEnv)
		if err != nil {
			return nil, err
		}
		build.Dir = consoleDir
		build.Stdout = os.Stdout
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			if reserved != nil {
				_ = reserved.Close()
			}
			return nil, fmt.Errorf("build console: %w", err)
		}

		runtimeEnv := cloneEnvironmentOverrides(env)
		runtimeEnv["HOST"] = bindAddress
		runtimeEnv["PORT"] = strconv.Itoa(port)
		cmd, err = localteststack.ChildCommand(ctx, "bun", []string{"run", ".output/server/index.mjs"}, runtimeEnv)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		cmd, err = localteststack.ChildCommand(ctx, "bun", []string{"--bun", "vite", "dev", "--host", bindAddress, "--port", strconv.Itoa(port), "--strictPort"}, env)
		if err != nil {
			return nil, err
		}
	}
	cmd.Dir = consoleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// bun spawns the dev server as a child. Run the console in its own process
	// group and signal the whole group, so shutdown never orphans a server
	// that keeps rewriting console sources for later runs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return signalConsoleGroup(cmd, syscall.SIGKILL)
	}
	if reserved != nil {
		if err := reserved.Close(); err != nil {
			return nil, fmt.Errorf("release reserved console port %d: %w", port, err)
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start console process: %w", err)
	}

	done := make(chan struct{})
	proc := &consoleProcess{cmd: cmd, done: done}
	go func() {
		proc.waitErr = cmd.Wait()
		close(done)
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: consoleStartupTimeout, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		select {
		case <-done:
			if proc.waitErr == nil {
				return false, fmt.Errorf("console process exited before becoming healthy")
			}
			return false, fmt.Errorf("console process exited before becoming healthy: %w", proc.waitErr)
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}); err != nil {
		stopConsoleProcess(proc)
		return nil, fmt.Errorf("wait for console health at %s: %w", healthURL, err)
	}

	return proc, nil
}

func cloneEnvironmentOverrides(env map[string]string) map[string]string {
	cloned := make(map[string]string, len(env)+2)
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}

func randomSecret(byteLength int) (string, error) {
	secret := make([]byte, byteLength)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func signingSecret(ctx context.Context, server *controlplane.Server, scope string) (string, error) {
	keys := server.SigningKeys()
	if keys == nil {
		return "", fmt.Errorf("signing keys are not initialized")
	}
	secret, err := keys.ActiveSecret(ctx, scope)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

func pickLoopbackPort() (int, error) {
	listener, port, err := reservePort("127.0.0.1")
	if err != nil {
		return 0, err
	}
	_ = listener.Close()
	return port, nil
}

func reservePort(bindAddress string) (net.Listener, int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(bindAddress, "0"))
	if err != nil {
		return nil, 0, err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return listener, addr.Port, nil
}

func waitForIngressDashboard(ctx context.Context, healthURL string) error {
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	})
}

func appendUniqueStrings(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return items
		}
	}
	return append(items, value)
}

func writeSummary(path string, summary stackSummary) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func startupInterrupted(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	log.Printf("startup interrupted: %v", ctx.Err())
	return true
}

func stopConsoleProcess(proc *consoleProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	select {
	case <-proc.done:
		return
	default:
	}
	_ = signalConsoleGroup(proc.cmd, syscall.SIGTERM)
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		_ = signalConsoleGroup(proc.cmd, syscall.SIGKILL)
		<-proc.done
	}
	// The group leader can exit before its children; sweep what is left.
	_ = signalConsoleGroup(proc.cmd, syscall.SIGKILL)
}

func signalConsoleGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

func waitForLocalAgent(ctx context.Context, server *controlplane.Server, agentID string, runErrCh <-chan error) error {
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for {
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("agent %s did not connect within 15s", agentID)
			}
			return pollCtx.Err()
		case err := <-runErrCh:
			if err == nil || ctx.Err() != nil {
				return context.Canceled
			}
			return fmt.Errorf("agent %s exited before connecting: %w", agentID, err)
		default:
		}

		healthy, err := server.HasHealthyAgent(pollCtx, agentID)
		if err != nil {
			return err
		}
		if healthy {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func monitorBackgroundComponent(ctx context.Context, stop context.CancelFunc, name string, runErrCh <-chan error) {
	err := <-runErrCh
	if err == nil || ctx.Err() != nil {
		return
	}
	log.Printf("%s exited: %v", name, err)
	if stop != nil {
		stop()
	}
}
