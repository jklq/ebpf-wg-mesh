package localteststack

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

var hostGatewayIPLabel = regexp.MustCompile(`(?m)host-gateway-ip:\s*([0-9a-fA-F:.]+)`)

func ResolveDockerHostGateway(ctx context.Context) (string, error) {
	if override := strings.TrimSpace(os.Getenv("LOCALTESTSTACK_DOCKER_HOST_GATEWAY")); override != "" {
		if ip := net.ParseIP(override); ip == nil {
			return "", fmt.Errorf("LOCALTESTSTACK_DOCKER_HOST_GATEWAY must be an IP address")
		}
		return override, nil
	}
	if ip, err := dockerHostGatewayFromBuildx(ctx); err == nil && ip != "" {
		return ip, nil
	}
	if ip, err := dockerHostGatewayFromContainer(ctx); err == nil && ip != "" {
		return ip, nil
	}
	return "host.docker.internal", nil
}

func dockerHostGatewayFromBuildx(ctx context.Context) (string, error) {
	cmd, err := ChildCommand(ctx, "docker", []string{"buildx", "inspect"}, nil)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	match := hostGatewayIPLabel.FindSubmatch(out)
	if len(match) != 2 {
		return "", fmt.Errorf("host-gateway-ip not found in docker buildx inspect")
	}
	ip := strings.TrimSpace(string(match[1]))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid host-gateway-ip %q", ip)
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return "", fmt.Errorf("host-gateway-ip %q is not IPv4", ip)
	}
	return ip, nil
}

func dockerHostGatewayFromContainer(ctx context.Context) (string, error) {
	cmd, err := ChildCommand(ctx, "docker", []string{
		"run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"alpine",
		"getent", "ahostsv4", "host.docker.internal",
	}, nil)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve host-gateway via container: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty host-gateway lookup")
	}
	ip := fields[0]
	if net.ParseIP(ip) == nil || net.ParseIP(ip).To4() == nil {
		return "", fmt.Errorf("invalid host-gateway IPv4 %q", ip)
	}
	return ip, nil
}
