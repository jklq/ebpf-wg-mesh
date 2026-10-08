package productionops

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

type recoveredFiles struct {
	Point            string            `json:"point"`
	Installation     string            `json:"installation"`
	Release          string            `json:"release"`
	StateKey         string            `json:"stateKey"`
	OperationsConfig string            `json:"operationsConfig"`
	RecoveryConfig   string            `json:"recoveryConfig"`
	State            string            `json:"state"`
	Secrets          string            `json:"secrets"`
	Keyring          string            `json:"keyring"`
	ConsoleKeys      []string          `json:"consoleKeys"`
	Tools            map[string]string `json:"tools"`
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
	selected := map[string]bool{}
	for _, requirement := range point.Installer {
		selected[requirement.Kind+"/"+requirement.ID] = true
	}
	for _, d := range point.Dependencies {
		if (d.Kind == "installation" || d.Kind == "release" || d.Kind == "deployment-state" || d.Kind == "external-secret") && !selected[d.Kind+"/"+d.ID] {
			continue
		}
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
	if err := hydrateRecovered(ctx, c, point, &state, &result, destination); err != nil {
		return result, err
	}
	return result, saveJSON(filepath.Join(destination, "recovered-files.json"), result)
}

func hydrateRecovered(ctx context.Context, independent recovery.Config, point recovery.Point, state *deploy.State, result *recoveredFiles, destination string) error {
	var bundle map[string][]byte
	if err := privateJSON(result.Secrets, &bundle); err != nil {
		return err
	}
	relocated := func(path string) string {
		if path == "" {
			return ""
		}
		return filepath.Join(destination, "files", strings.TrimPrefix(path, "/"))
	}
	for path, data := range bundle {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("protected input path escapes recovery")
		}
		if err := writePrivate(relocated(path), data); err != nil {
			return err
		}
	}
	var installation deploy.Installation
	if err := privateJSON(result.Installation, &installation); err != nil {
		return err
	}
	oldConfig := installation.OperationsConfig
	var config Config
	if err := privateJSON(relocated(oldConfig), &config); err != nil {
		return err
	}
	var rec recovery.Config
	if err := privateJSON(relocated(config.RecoveryConfig), &rec); err != nil {
		return err
	}
	tool := func(name string) (string, error) {
		path := result.Tools[installation.Release+"/tool/"+name+"/"+runtime.GOARCH]
		if path == "" {
			return "", fmt.Errorf("protected native tool %s/%s is missing", name, runtime.GOARCH)
		}
		return path, nil
	}
	rewriteInstallation := func(i *deploy.Installation) {
		for name, ref := range i.Secrets {
			ref.File = relocated(ref.File)
			i.Secrets[name] = ref
		}
		for n := range i.Hosts {
			i.Hosts[n].SSH.KnownHosts = relocated(i.Hosts[n].SSH.KnownHosts)
		}
		for n := range i.Recovery.Hosts {
			i.Recovery.Hosts[n].SSH.KnownHosts = relocated(i.Recovery.Hosts[n].SSH.KnownHosts)
		}
		for n := range i.OperationsInputs {
			i.OperationsInputs[n] = relocated(i.OperationsInputs[n])
		}
		i.OperationsConfig = relocated(i.OperationsConfig)
		i.Recovery.Inventory = relocated(i.Recovery.Inventory)
	}
	rewriteInstallation(&installation)
	if state.Policy != nil {
		rewriteInstallation(state.Policy)
	}
	config.StateDirectory = filepath.Join(destination, "runtime")
	config.Database.CertificateDirectory = filepath.Join(destination, "database-certificates")
	config.Database.URLFile = filepath.Join(destination, "database-url")
	config.Database.BackupURIFile = relocated(config.Database.BackupURIFile)
	config.Console.TokenKeyFile = filepath.Join(destination, "console-token.key")
	if len(result.ConsoleKeys) != 1 {
		return fmt.Errorf("selected point requires exactly one console token encryption key")
	}
	token, err := os.ReadFile(result.ConsoleKeys[0])
	if err != nil {
		return err
	}
	if err := writePrivate(config.Console.TokenKeyFile, token); err != nil {
		return err
	}
	config.Database.Binary, err = tool("cockroachdb")
	if err != nil {
		return err
	}
	config.Console.AdminBinary, err = tool("console-admin")
	if err != nil {
		return err
	}
	config.WildcardCertificate = relocated(config.WildcardCertificate)
	config.WildcardKey = relocated(config.WildcardKey)
	config.SourceConfig = relocated(config.SourceConfig)
	config.MonitorTokenFile = relocated(config.MonitorTokenFile)
	for id, f := range config.Fences {
		f.CredentialsFile = relocated(f.CredentialsFile)
		f.CAFile = relocated(f.CAFile)
		config.Fences[id] = f
	}
	for name, s := range config.Storage {
		s.S3.CredentialsFile = relocated(s.S3.CredentialsFile)
		s.S3.CAFile = relocated(s.S3.CAFile)
		config.Storage[name] = s
	}
	config.RecoveryConfig = relocated(config.RecoveryConfig)
	rec.RecoveryKeyFile = independent.RecoveryKeyFile
	rec.KeyringFile = result.Keyring
	rec.DatabaseURLFile = config.Database.URLFile
	rec.Storage.CredentialsFile = relocated(rec.Storage.CredentialsFile)
	rec.Storage.CAFile = relocated(rec.Storage.CAFile)
	if rec.Images.CertificateDirectory != "" {
		rec.Images.CertificateDirectory = filepath.Join(destination, "files", strings.TrimPrefix(rec.Images.CertificateDirectory, "/"))
	}
	rec.Images.AuthFile = relocated(rec.Images.AuthFile)
	rec.Images.Binary, err = tool("skopeo")
	if err != nil {
		return err
	}
	operations, err := tool("operations")
	if err != nil {
		return err
	}
	digest, _, err := recovery.FileDigest(operations)
	if err != nil {
		return err
	}
	rec.DrillCommand = []string{operations, "offline-recovery"}
	rec.DrillCommandDigest = digest
	rec.Files = nil // The restored operation rebuilds protection from actual new state.
	installation.OperationsInputs = []string{config.Console.TokenKeyFile, rec.KeyringFile, independent.RecoveryKeyFile}
	for path := range bundle {
		installation.OperationsInputs = append(installation.OperationsInputs, relocated(path))
	}
	// The administration host uses its staged native executables, while the
	// independent operator CLI uses materialized tools from the same pinned point.
	remoteRec := rec
	toolRoot := "/opt/ebpf-wg-mesh/" + installation.ID + "/" + installation.Release + "/tools/"
	config.Database.Binary = toolRoot + "cockroachdb"
	config.Console.AdminBinary = toolRoot + "console-admin"
	remoteRec.Images.Binary = toolRoot + "skopeo"
	remoteRec.DrillCommand = []string{toolRoot + "operations", "offline-recovery"}
	if config.SourceConfig != "" {
		b, err := os.ReadFile(config.SourceConfig)
		if err != nil {
			return err
		}
		var document any
		if err = json.Unmarshal(b, &document); err != nil {
			return err
		}
		var rewrite func(any) any
		rewrite = func(v any) any {
			switch x := v.(type) {
			case string:
				if _, exists := bundle[x]; exists {
					return relocated(x)
				}
				return x
			case map[string]any:
				for key, value := range x {
					x[key] = rewrite(value)
				}
				return x
			case []any:
				for n, value := range x {
					x[n] = rewrite(value)
				}
				return x
			default:
				return v
			}
		}
		if err = saveJSON(config.SourceConfig, rewrite(document)); err != nil {
			return err
		}
	}
	if err = saveJSON(config.RecoveryConfig, remoteRec); err != nil {
		return err
	}
	installation.OperationsInputs = append(installation.OperationsInputs, config.RecoveryConfig, installation.OperationsConfig)

	// Keep the externally retained decryption key external, rather than requiring
	// the lost disk's original path or re-encrypting it under itself.
	ref, exists := installation.Secrets[installation.Backup.RecoveryKey]
	if !exists || installation.Backup.RecoveryKey == "" {
		return fmt.Errorf("protected installation is missing the independent recovery-key reference")
	}
	ref.File = independent.RecoveryKeyFile
	installation.Secrets[installation.Backup.RecoveryKey] = ref
	if state.Policy != nil {
		if state.Policy.Secrets == nil {
			state.Policy.Secrets = map[string]deploy.SecretRef{}
		}
		state.Policy.Secrets[installation.Backup.RecoveryKey] = ref
		state.Policy.OperationsInputs = installation.OperationsInputs
	}
	result.OperationsConfig = installation.OperationsConfig
	result.RecoveryConfig = filepath.Join(destination, "operator-recovery.json")
	if err = config.Validate(); err != nil {
		return err
	}
	for path, value := range map[string]any{result.Installation: installation, result.OperationsConfig: config, result.RecoveryConfig: rec} {
		if err = saveJSON(path, value); err != nil {
			return err
		}
	}
	result.StateKey = filepath.Join(destination, "deployment.key")
	key, err := os.ReadFile(result.StateKey)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		if err = writePrivate(result.StateKey, key); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	defer clear(key)
	result.State = filepath.Join(destination, "deployment.state")
	store, err := deploy.OpenState(result.State, key)
	if err != nil {
		return err
	}
	defer store.Close()
	if state.Bindings == nil {
		state.Bindings = map[string]deploy.Binding{}
	}
	if err = store.Write(*state); err != nil {
		return err
	}
	actual, err := store.Read()
	if err != nil {
		return err
	}
	if deploy.Digest(actual) != deploy.Digest(*state) {
		return fmt.Errorf("recovered state readback differs")
	}
	return nil
}
