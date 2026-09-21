package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func generateSSHKey(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return runCommand(ctx, ".", os.Environ(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
}

const (
	vmControlPlaneDBURL    = "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"
	vmControlPlaneStateDir = "/var/lib/ebpf-wg-mesh/controlplane"
)

func signingKeysCommand(subcommand string) string {
	return fmt.Sprintf("/opt/ebpf-wg-mesh/controlplane signing-keys %s -db-url %s -state-dir %s",
		subcommand, shellQuote(vmControlPlaneDBURL), shellQuote(vmControlPlaneStateDir))
}

func fetchClientIdentity(ctx context.Context, keyPath, host string) (clientIdentity, error) {
	// Mint a dashboard mTLS client cert from the shared CA.
	// CN must match the controlplane -dashboard-service-caller-id default ("dashboard").
	remote := signingKeysCommand(fmt.Sprintf("issue-client-cert -caller-class dashboard -caller-id %s", shellQuote(vmDashboardCallerID)))
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, remote)...)
	output, err := cmd.Output()
	if err != nil {
		return clientIdentity{}, err
	}
	var identity clientIdentity
	if err := json.Unmarshal(output, &identity); err != nil {
		return clientIdentity{}, err
	}
	return identity, nil
}

// exportSigningMaterial fetches a trust bundle (ECDSA scopes) or active
// secret (HMAC scopes) from the control plane's shared signing keys.
func exportSigningMaterial(ctx context.Context, keyPath, host, scope string) ([]byte, error) {
	return runRemoteCommand(ctx, keyPath, host, signingKeysCommand("export -scope "+shellQuote(scope)))
}

func runRemoteScript(ctx context.Context, keyPath, host, scriptPath string, env map[string]string) error {
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		return err
	}
	cmdLine := ""
	for key, value := range env {
		cmdLine += key + "=" + shellQuote(value) + " "
	}
	cmdLine += "bash -s"
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, cmdLine)...)
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runRemoteCommand(ctx context.Context, keyPath, host, remoteCmd string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, remoteCmd)...)
	return cmd.CombinedOutput()
}

func copyFile(ctx context.Context, keyPath, source, host, destination string) error {
	args := append(scpBaseArgs(keyPath), source, fmt.Sprintf("root@%s:%s", host, destination))
	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyFromRemote(ctx context.Context, keyPath, host, source, destination string) error {
	args := append(scpBaseArgs(keyPath), fmt.Sprintf("root@%s:%s", host, source), destination)
	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sshArgs(keyPath, host, remoteCmd string) []string {
	args := append([]string(nil), sshBaseArgs(keyPath)...)
	args = append(args, "root@"+host, remoteCmd)
	return args
}

func sshBaseArgs(keyPath string) []string {
	return []string{
		"-i", keyPath,
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
}

func scpBaseArgs(keyPath string) []string {
	return append([]string(nil), sshBaseArgs(keyPath)...)
}
