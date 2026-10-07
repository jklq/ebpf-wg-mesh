package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

type recoveredFiles struct {
	Point        string            `json:"point"`
	Installation string            `json:"installation"`
	Release      string            `json:"release"`
	State        string            `json:"state"`
	Secrets      string            `json:"secrets"`
	Keyring      string            `json:"keyring"`
	ConsoleKeys  []string          `json:"consoleKeys"`
	Tools        map[string]string `json:"tools"`
}

// RecoverFiles is the independent entry point after losing the operator disk,
// vault and release server. It needs only the independent storage configuration,
// its credentials and the separately retained recovery decryption key.
func RecoverFiles(ctx context.Context, configPath, backup, destination string) (recoveredFiles, error) {
	result := recoveredFiles{Tools: map[string]string{}}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return result, fmt.Errorf("recovery destination must be absolute and clean")
	}
	c, s, err := recoveryConfig(configPath)
	if err != nil {
		return result, err
	}
	defer clear(s.RecoveryKey)
	object, err := s.ResolvePoint(ctx, c.Storage.Bucket, backup)
	if err != nil {
		return result, err
	}
	point, err := s.ReadPoint(ctx, object)
	if err != nil {
		return result, err
	}
	if report := s.Verify(ctx, point, true); !report.Complete {
		return result, fmt.Errorf("independent point verification failed: %v %v", report.Missing, report.Failures)
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return result, err
	}
	result.Point = filepath.Join(destination, "point.json")
	if err := saveJSON(result.Point, point); err != nil {
		return result, err
	}
	result.Keyring = filepath.Join(destination, "keyring.json")
	if err := s.RecoverKeyring(ctx, point, result.Keyring); err != nil {
		return result, err
	}
	for _, d := range point.Dependencies {
		name := strings.TrimPrefix(recovery.Digest([]byte(d.Kind+"/"+d.ID)), "sha256:")
		target := filepath.Join(destination, "dependencies", name)
		switch d.Kind {
		case "installation", "release", "deployment-state", "external-secret", "console-key", "tool":
			secret := d.Kind == "deployment-state" || d.Kind == "external-secret" || d.Kind == "console-key"
			if err := s.Materialize(ctx, d, target, secret); err != nil {
				return result, err
			}
		default:
			continue
		}
		switch d.Kind {
		case "installation":
			result.Installation = target
		case "release":
			result.Release = target
		case "deployment-state":
			result.State = target
		case "external-secret":
			if strings.HasPrefix(d.ID, "installer-secrets/") {
				result.Secrets = target
			}
		case "console-key":
			result.ConsoleKeys = append(result.ConsoleKeys, target)
		case "tool":
			if err := os.Chmod(target, 0700); err != nil {
				return result, err
			}
			result.Tools[d.ID] = target
		}
	}
	if result.Installation == "" || result.Release == "" || result.State == "" || result.Secrets == "" {
		return result, fmt.Errorf("point lacks complete installer dependencies")
	}
	var state deploy.State
	b, err := os.ReadFile(result.State)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(b, &state); err != nil || state.Version != 1 {
		return result, fmt.Errorf("protected installer state is not a deployment snapshot")
	}
	// StateStore's encryption uses an independently retained operator key. The
	// materialized snapshot is private JSON for inspection/import, never an attempt
	// file accepted as proof that a restore has succeeded.
	return result, saveJSON(filepath.Join(destination, "recovered-files.json"), result)
}
