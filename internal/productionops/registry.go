package productionops

import (
	"fmt"
	"net/url"
	"os"

	"ebof-wg-mesh/internal/deploy"
	"go.yaml.in/yaml/v3"
	"gopkg.in/ini.v1"
)

func (r *Runner) registryConfiguration(pl deploy.Placement) ([]byte, error) {
	if r.Config.RegistryRealm == "" || r.Config.RegistryService == "" {
		return nil, fmt.Errorf("registry token realm and service must be selected")
	}
	names := r.Plan.Installation.Components[deploy.Registry].Storage
	if len(names) != 1 {
		return nil, fmt.Errorf("registry needs exactly one durable storage service")
	}
	name := names[0]
	s := r.Plan.Installation.Storage[name]
	c := r.Config.Storage[name]
	storage := map[string]any{}
	switch c.Kind {
	case "directory":
		storage["filesystem"] = map[string]any{"rootdirectory": s.Path}
	case "s3":
		raw, err := os.ReadFile(c.S3.CredentialsFile)
		if err != nil {
			return nil, err
		}
		file, err := ini.Load(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid S3 credentials file")
		}
		section, err := file.GetSection(c.S3.Profile)
		if err != nil {
			return nil, err
		}
		access, secret := section.Key("aws_access_key_id").String(), section.Key("aws_secret_access_key").String()
		if access == "" || secret == "" {
			return nil, fmt.Errorf("registry S3 requires explicit access credentials")
		}
		endpoint, err := url.Parse(c.S3.Endpoint)
		if err != nil || endpoint.Scheme != "https" {
			return nil, fmt.Errorf("registry S3 requires HTTPS")
		}
		options := map[string]any{"accesskey": access, "secretkey": secret, "region": c.S3.Region, "regionendpoint": c.S3.Endpoint, "bucket": c.S3.Bucket, "rootdirectory": c.S3.Prefix, "secure": true, "v4auth": true}
		if token := section.Key("aws_session_token").String(); token != "" {
			options["sessiontoken"] = token
		}
		storage["s3"] = options
	default:
		return nil, fmt.Errorf("registry storage needs directory or S3 selection")
	}
	return yaml.Marshal(map[string]any{"version": 0.1, "storage": storage, "http": map[string]any{"addr": ":5000"}, "auth": map[string]any{"token": map[string]any{"realm": r.Config.RegistryRealm, "service": r.Config.RegistryService, "issuer": "ebpf-wg-mesh", "rootcertbundle": cfgDir(r.Plan, pl) + "/registry-trust.crt"}}})
}
