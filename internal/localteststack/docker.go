package localteststack

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

type DockerRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type ExecDockerRunner struct{}

func (ExecDockerRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := ChildCommand(ctx, "docker", args, nil)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("docker %s: %s", strings.Join(sanitizeDockerArgsForError(args), " "), message)
	}
	return stdout.Bytes(), nil
}

func sanitizeDockerArgsForError(args []string) []string {
	sanitized := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--env" || arg == "-e" {
			sanitized = append(sanitized, arg)
			if i+1 < len(args) {
				i++
				sanitized = append(sanitized, redactEnvAssignment(args[i]))
			}
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--env="); ok {
			sanitized = append(sanitized, "--env="+redactEnvAssignment(value))
			continue
		}
		if value, ok := strings.CutPrefix(arg, "-e="); ok {
			sanitized = append(sanitized, "-e="+redactEnvAssignment(value))
			continue
		}
		sanitized = append(sanitized, arg)
	}
	return sanitized
}

func redactEnvAssignment(assignment string) string {
	if key, _, ok := strings.Cut(assignment, "="); ok {
		return key + "=" + redactedValue
	}
	return redactedValue
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

func listDockerNetworks(ctx context.Context, runner DockerRunner) ([]string, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	out, err := runner.Run(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Fields(string(out)) {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
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

func CleanupStaleLocalteststackContainers(ctx context.Context, runner DockerRunner) (int, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	ids := make(map[string]struct{})
	labeled, err := listContainerIDs(ctx, runner, "label="+localRuntimeLabel+"="+localRuntimeManagedBy)
	if err != nil {
		return 0, err
	}
	for _, id := range labeled {
		ids[id] = struct{}{}
	}
	prefixed, err := listContainerIDs(ctx, runner, "name=localteststack-")
	if err != nil {
		return 0, err
	}
	for _, id := range prefixed {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := []string{"rm", "--force"}
	for id := range ids {
		args = append(args, id)
	}
	if _, err := runner.Run(ctx, args...); err != nil {
		if isDockerMissingObjectError(err) {
			return len(ids), nil
		}
		return 0, err
	}
	return len(ids), nil
}

func listContainerIDs(ctx context.Context, runner DockerRunner, filters ...string) ([]string, error) {
	args := []string{"ps", "-aq"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	out, err := runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	ids := make([]string, 0, len(fields))
	for _, id := range fields {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func isDockerActiveEndpointsError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "active endpoints")
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
