package localteststack

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type DockerRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type ExecDockerRunner struct{}

func (ExecDockerRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("docker %s: %s", strings.Join(args, " "), message)
	}
	return stdout.Bytes(), nil
}

func EnsureDockerNetwork(ctx context.Context, runner DockerRunner, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("docker network name is required")
	}
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if _, err := runner.Run(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	if _, err := runner.Run(ctx, "network", "create", name); err != nil {
		if _, inspectErr := runner.Run(ctx, "network", "inspect", name); inspectErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func RemoveDockerNetwork(ctx context.Context, runner DockerRunner, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	_, err := runner.Run(ctx, "network", "rm", name)
	if isDockerMissingObjectError(err) {
		return nil
	}
	return err
}

func isDockerMissingObjectError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such object") ||
		strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such network") ||
		strings.Contains(message, "not found")
}
