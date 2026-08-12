package agent

import (
	"fmt"
	"path/filepath"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func isManagedDashboardService(svc *agentv1.DesiredService) bool {
	return svc.GetSpec().GetRuntime().GetEnv()[managedDashboardSecretMarker] == managedDashboardSecretValue
}

const maxRuntimeIDLength = 128

const (
	managedDashboardSecretMarker = "PLATFORM_MANAGED_SECRET_SET"
	managedDashboardSecretValue  = "dashboard"
	managedDashboardSecretMount  = "/run/secrets/dashboard"
)

func validateRuntimeID(kind, id string) error {
	if id == "" || len(id) > maxRuntimeIDLength {
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	return nil
}

func runtimeChildPath(base, kind, id string) (string, error) {
	if err := validateRuntimeID(kind, id); err != nil {
		return "", err
	}
	cleanBase, err := filepath.Abs(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("resolve %s base: %w", kind, err)
	}
	path := filepath.Clean(filepath.Join(cleanBase, id))
	rel, err := filepath.Rel(cleanBase, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q escapes runtime directory", kind, id)
	}
	return path, nil
}
