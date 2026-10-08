package productionops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

// externalInputs enumerates the service-selection closure. Missing dependencies
// fail protection before a cutover; generated keys are protected separately.
func (r *Runner) externalInputs(c recovery.Config) (map[string][]byte, error) {
	paths := []string{r.Plan.Installation.OperationsConfig, r.Config.RecoveryConfig, c.Storage.CredentialsFile, c.Storage.CAFile, c.Images.AuthFile, r.Config.WildcardCertificate, r.Config.WildcardKey, r.Config.SourceConfig, r.Config.MonitorTokenFile, r.Config.Database.BackupURIFile}
	paths = append(paths, r.Plan.Installation.OperationsInputs...)
	// Once generated, native database authority and installer client credentials
	// belong to the same independently encrypted closure as service secrets.
	if _, err := os.Stat(r.Config.Database.URLFile); err == nil {
		paths = append(paths, r.Config.Database.URLFile)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Stat(r.Config.Database.CertificateDirectory); err == nil {
		if err := filepath.WalkDir(r.Config.Database.CertificateDirectory, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("database key closure contains a nonregular file")
			}
			paths = append(paths, path)
			return nil
		}); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, f := range r.Config.Fences {
		paths = append(paths, f.CredentialsFile, f.CAFile)
	}
	for _, h := range append(append([]deploy.Host{}, r.Plan.Installation.Hosts...), r.Plan.Installation.Recovery.Hosts...) {
		paths = append(paths, h.SSH.KnownHosts)
	}
	for name, ref := range r.Plan.Installation.Secrets {
		if name == r.Plan.Installation.Backup.RecoveryKey || ref.File == r.Config.Database.URLFile || ref.File == r.Config.Console.TokenKeyFile || ref.File == c.KeyringFile {
			continue
		}
		if !strings.Contains(ref.File, "{") {
			paths = append(paths, ref.File)
			continue
		}
		for _, pl := range r.Plan.Placements {
			expanded := strings.NewReplacer("{instance}", pl.Instance, "{host}", pl.Host, "{configDir}", cfgDir(r.Plan, pl), "{stateDir}", dataDir(r.Plan, pl)).Replace(ref.File)
			if strings.Contains(expanded, "{") {
				return nil, fmt.Errorf("unresolved external credential %s", name)
			}
			paths = append(paths, expanded)
		}
	}
	for _, storage := range r.Config.Storage {
		if storage.Kind == "s3" {
			paths = append(paths, storage.S3.CredentialsFile, storage.S3.CAFile)
		}
	}
	if c.Images.CertificateDirectory != "" {
		err := filepath.WalkDir(c.Images.CertificateDirectory, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("image TLS directory contains non-regular material")
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Source adapters name their credential/config files in JSON. Protect those
	// explicit paths as well, without treating endpoint strings as files.
	if r.Config.SourceConfig != "" {
		b, err := os.ReadFile(r.Config.SourceConfig)
		if err != nil {
			return nil, err
		}
		var document any
		if err = json.Unmarshal(b, &document); err != nil {
			return nil, err
		}
		var collect func(any)
		collect = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, v := range x {
					if s, ok := v.(string); ok && (strings.HasSuffix(strings.ToLower(k), "file") || k == "credentials") && filepath.IsAbs(s) {
						paths = append(paths, s)
					}
					collect(v)
				}
			case []any:
				for _, v := range x {
					collect(v)
				}
			}
		}
		collect(document)
	}
	if _, err := os.Stat(r.Plan.Installation.Recovery.Inventory); err == nil {
		paths = append(paths, r.Plan.Installation.Recovery.Inventory)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	bundle := map[string][]byte{}
	for _, path := range paths {
		if path == "" || path == c.RecoveryKeyFile {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("invalid dependency path")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("external dependency %s unavailable: %w", path, err)
		}
		bundle[path] = b
	}
	return bundle, nil
}
