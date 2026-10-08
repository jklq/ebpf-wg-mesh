// Package productionops implements deployment and recovery without platform RPC
// authority. Configuration names services and credentials, never lifecycle code.
package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Config struct {
	BuilderSandbox      config.BuilderSandboxConfig `json:"builderSandbox,omitempty"`
	Fences              map[string]RedfishFence     `json:"fences,omitempty"`
	CompletionHosts     []string                    `json:"completionHosts,omitempty"`
	Version             int                         `json:"version"`
	StateDirectory      string                      `json:"stateDirectory"`
	RecoveryConfig      string                      `json:"recoveryConfig"`
	Database            DatabaseConfig              `json:"database"`
	Console             ConsoleConfig               `json:"console"`
	Probes              map[deploy.Role]Probe       `json:"probes"`
	EndpointProbes      map[string]Probe            `json:"endpointProbes"`
	Storage             map[string]StoreConfig      `json:"storage"`
	WildcardCertificate string                      `json:"wildcardCertificate"`
	WildcardKey         string                      `json:"wildcardKey"`
	PlatformDomain      string                      `json:"platformDomain"`
	InternalServerName  string                      `json:"internalServerName"`
	RegistryRealm       string                      `json:"registryRealm"`
	RegistryService     string                      `json:"registryService"`
	SourceConfig        string                      `json:"sourceConfig,omitempty"`
	MonitorTokenFile    string                      `json:"monitorTokenFile,omitempty"`
}
type DatabaseConfig struct {
	Binary               string `json:"binary"`
	Address              string `json:"address"`
	CertificateDirectory string `json:"certificateDirectory"`
	URLFile              string `json:"urlFile"`
	Name                 string `json:"name"`
	BackupURIFile        string `json:"backupURIFile"`
}
type ConsoleConfig struct {
	AdminBinary  string `json:"adminBinary"`
	Schema       string `json:"schema"`
	TokenKeyFile string `json:"tokenKeyFile"`
}

// HTTP probes use a TLS trust root and optional client identity. Plain HTTP is
// allowed only for a health listener on loopback, inspected on its owning host.
type Probe struct {
	ServerName          string `json:"serverName,omitempty"`
	URL                 string `json:"url"`
	RuntimeURL          string `json:"runtimeURL,omitempty"`
	CAFile              string `json:"caFile,omitempty"`
	CertFile            string `json:"certFile,omitempty"`
	KeyFile             string `json:"keyFile,omitempty"`
	Status              int    `json:"status"`
	BodyContains        string `json:"bodyContains,omitempty"`
	AuthorizationMethod string `json:"authorizationMethod,omitempty"`
	AuthorizationBody   string `json:"authorizationBody,omitempty"`
	AuthorizationURL    string `json:"authorizationURL,omitempty"`
	UnauthorizedStatus  int    `json:"unauthorizedStatus,omitempty"`
}
type StoreConfig struct {
	Kind string                 `json:"kind"` // local directory, or versioned object storage
	S3   recovery.StorageConfig `json:"s3,omitempty"`
}

type Runner struct {
	Plan             deploy.Plan
	Config           Config
	Remote           deploy.Remote
	Adapter          func(deploy.Installation, deploy.Host) (deploy.Adapter, error)
	HTTP             *http.Client
	RecoveryProgress *deploy.RecoveryProgress
}

func privateJSON(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s must be a private regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 8<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("configuration must contain one JSON document")
	}
	return nil
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("operations configuration version must be 1")
	}
	for _, path := range []string{c.StateDirectory, c.RecoveryConfig, c.Database.Binary, c.Database.CertificateDirectory, c.Database.URLFile, c.Database.BackupURIFile, c.Console.AdminBinary, c.Console.TokenKeyFile, c.WildcardCertificate, c.WildcardKey} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("operations file paths must be absolute and clean")
		}
	}
	identifier := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	if !identifier.MatchString(c.Database.Name) || !identifier.MatchString(c.Console.Schema) || c.InternalServerName == "" || c.PlatformDomain == "" {
		return fmt.Errorf("database, console and TLS service identities are required")
	}
	for _, probe := range c.Probes {
		if err := probe.Validate(); err != nil {
			return err
		}
	}
	for _, probe := range c.EndpointProbes {
		if err := probe.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (p Probe) Validate() error {
	target := strings.NewReplacer("{address}", "127.0.0.1", "{socketHost}", "127.0.0.1", "{instance}", "instance").Replace(p.URL)
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || p.Status < 100 || p.Status > 599 {
		return fmt.Errorf("invalid HTTP probe")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()) {
		return fmt.Errorf("HTTP health probes require literal loopback; service probes require HTTPS")
	}
	if (p.CertFile == "") != (p.KeyFile == "") {
		return fmt.Errorf("probe client certificate and key must be paired")
	}
	if p.AuthorizationMethod != "" && p.AuthorizationMethod != http.MethodPost && p.AuthorizationMethod != http.MethodGet {
		return fmt.Errorf("authorization inspection only supports read-only GET or Connect POST")
	}
	if p.AuthorizationURL != "" {
		a, err := url.Parse(strings.NewReplacer("{address}", "127.0.0.1", "{socketHost}", "127.0.0.1", "{instance}", "instance").Replace(p.AuthorizationURL))
		if err != nil || a.Scheme != "https" || a.User != nil || a.Host == "" || (p.UnauthorizedStatus != 401 && p.UnauthorizedStatus != 403 && p.UnauthorizedStatus != 503) {
			return fmt.Errorf("authorization probes require HTTPS and a rejection status")
		}
	}
	return nil
}

func (r *Runner) db(ctx context.Context, system bool) (*sql.DB, error) {
	b, err := os.ReadFile(r.Config.Database.URLFile)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil || u.Scheme != "postgresql" || u.Query().Get("sslmode") != "verify-full" {
		return nil, fmt.Errorf("operator database connection must use verify-full TLS")
	}
	if system {
		// Full-cluster restore replaces defaultdb. Administrative recovery queries
		// must stay connected to the system database throughout that transition.
		u.Path = "/system"
	} else {
		u.Path = "/" + r.Config.Database.Name
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(probeCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("operator database unavailable: %w", err)
	}
	return db, nil
}

func (r *Runner) defaults() {
	if r.Remote == nil {
		r.Remote = deploy.SSHRemote{}
	}
	if r.Adapter == nil {
		r.Adapter = deploy.NewAdapter
	}
	if r.HTTP == nil {
		r.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
}
func cfgDir(p deploy.Plan, pl deploy.Placement) string {
	return "/etc/ebpf-wg-mesh/" + p.Installation.ID + "/" + pl.Instance
}
func dataDir(p deploy.Plan, pl deploy.Placement) string {
	return "/var/lib/ebpf-wg-mesh/" + p.Installation.ID + "/" + pl.Instance
}
func unit(p deploy.Plan, pl deploy.Placement) string {
	return "platform-" + p.Installation.ID + "-" + pl.Instance + ".service"
}
func shell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (r *Runner) host(ctx context.Context, p deploy.Plan, id string) (deploy.Host, error) {
	h, ok := p.Installation.Host(id)
	if !ok {
		return h, fmt.Errorf("unknown host %s", id)
	}
	if p.Previous != nil {
		if b := p.Previous.Bindings[id]; b.ServerID != "" && h.Binding.ServerID == "" {
			h.Binding = b
		}
	}
	a, err := r.Adapter(p.Installation, h)
	if err != nil {
		return h, err
	}
	s, found, err := a.Discover(ctx, p.Installation.ID, h)
	if err != nil {
		return h, err
	}
	if !found {
		return h, fmt.Errorf("host %s not found", id)
	}
	h.SSH.Address = strings.ReplaceAll(h.SSH.Address, "{providerAddress}", s.Address)
	return h, nil
}
func (r *Runner) remote(ctx context.Context, p deploy.Plan, pl deploy.Placement, script string) ([]byte, error) {
	h, err := r.host(ctx, p, pl.Host)
	if err != nil {
		return nil, err
	}
	return r.Remote.Run(ctx, p.Installation, h, script)
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".operations-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
