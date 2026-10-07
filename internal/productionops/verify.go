package productionops

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func probeHTTP(ctx context.Context, p Probe, authorization bool) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.CAFile != "" {
		b, err := os.ReadFile(p.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("invalid probe trust root")
		}
	}
	if !authorization && p.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	target, status := p.URL, p.Status
	if authorization {
		target, status = p.AuthorizationURL, p.UnauthorizedStatus
		if target == "" {
			return nil, fmt.Errorf("authorization rejection probe required")
		}
	}
	method, body := http.MethodGet, ""
	if authorization && p.AuthorizationMethod != "" {
		method = p.AuthorizationMethod
		body = p.AuthorizationBody
	}
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("service probe failed: %w", err)
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != status {
		return nil, fmt.Errorf("service probe returned HTTP %d; expected %d", response.StatusCode, status)
	}
	if !authorization && p.BodyContains != "" && !strings.Contains(string(b), p.BodyContains) {
		return nil, fmt.Errorf("service probe response lacks required content")
	}
	return b, nil
}
func Ready(ctx context.Context, role deploy.Role, instance, path string) error {
	if instance == "" {
		return fmt.Errorf("readiness instance required")
	}
	var p Probe
	if err := privateJSON(path, &p); err != nil {
		return err
	}
	_, err := probeHTTP(ctx, p, false)
	return err
}
func (r *Runner) expandProbe(p Probe, pl deploy.Placement) Probe {
	h, _ := r.Plan.Installation.Host(pl.Host)
	replace := strings.NewReplacer("{configDir}", cfgDir(r.Plan, pl), "{stateDir}", dataDir(r.Plan, pl), "{address}", h.Network.Address, "{instance}", pl.Instance)
	p.URL = replace.Replace(p.URL)
	p.RuntimeURL = replace.Replace(p.RuntimeURL)
	p.CAFile = replace.Replace(p.CAFile)
	p.CertFile = replace.Replace(p.CertFile)
	p.KeyFile = replace.Replace(p.KeyFile)
	p.AuthorizationURL = replace.Replace(p.AuthorizationURL)
	return p
}
func operationsBinary(p deploy.Plan) string {
	return "/opt/ebpf-wg-mesh/" + p.Installation.ID + "/" + p.Release.ID + "/tools/operations"
}
func (r *Runner) hostProbe(ctx context.Context, p deploy.Plan, pl deploy.Placement, probe Probe, authorization bool) ([]byte, error) {
	// The release tool inspects from the component's actual network namespace/host,
	// using its mounted credentials. No host-admin credentials leave this host.
	b, err := json.Marshal(probe)
	if err != nil {
		return nil, err
	}
	path := cfgDir(p, pl) + "/inspection-probe.json"
	command := shell(operationsBinary(p)) + " probe " + shell(path)
	if authorization {
		command += " unauthorized"
	}
	return r.remote(ctx, p, pl, remoteFile(path, b)+command+"\n")
}
func (r *Runner) storageStatus(ctx context.Context) (map[string]deploy.StorageStatus, error) {
	result := map[string]deploy.StorageStatus{}
	for name, storage := range r.Plan.Installation.Storage {
		databaseOnly := slices.Contains(r.Plan.Installation.Components[deploy.Database].Storage, name)
		for role, component := range r.Plan.Installation.Components {
			if role != deploy.Database && slices.Contains(component.Storage, name) {
				databaseOnly = false
			}
		}
		if databaseOnly {
			status, err := r.databaseStatus(ctx)
			if err != nil {
				return nil, err
			}
			result[name] = deploy.StorageStatus{Hosts: status.Members, Verified: true}
			continue
		}
		c, ok := r.Config.Storage[name]
		if !ok {
			return nil, fmt.Errorf("storage %s needs a service selection", name)
		}
		if c.Kind == "s3" {
			store := &recovery.S3{Config: c.S3}
			if err := store.VerifyActiveStore(ctx); err != nil {
				return nil, err
			}
			result[name] = deploy.StorageStatus{Hosts: append([]string{}, storage.Hosts...), Verified: true}
			continue
		}
		if c.Kind == "directory" && storage.Replicated {
			return nil, fmt.Errorf("directory readback cannot establish independent storage replicas; select a verified external object store")
		}
		if c.Kind != "directory" {
			return nil, fmt.Errorf("unsupported storage selection %s", c.Kind)
		}
		if len(storage.Hosts) == 0 {
			return nil, fmt.Errorf("storage has no serving hosts")
		}
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		filename := storage.Path + "/.platform-inspection-" + hex.EncodeToString(nonce)
		first := deploy.Placement{Host: storage.Hosts[0]}
		if _, err := r.remote(ctx, r.Plan, first, remoteFile(filename, nonce)+"sync -f "+shell(filename)+"\n"); err != nil {
			return nil, err
		}
		defer r.remote(context.WithoutCancel(ctx), r.Plan, first, "rm -f "+shell(filename)+"\n")
		status := deploy.StorageStatus{}
		for _, host := range storage.Hosts {
			if _, err := r.remote(ctx, r.Plan, deploy.Placement{Host: host}, verifyRemoteFile(filename, nonce)); err != nil {
				return nil, fmt.Errorf("storage %s is not shared and readable on %s: %w", name, host, err)
			}
			status.Hosts = append(status.Hosts, host)
		}
		status.Verified = true
		result[name] = status
	}
	return result, nil
}
func (r *Runner) productionVerify(ctx context.Context) error {
	if _, err := r.databaseStatus(ctx); err != nil {
		return err
	}
	if _, err := r.storageStatus(ctx); err != nil {
		return err
	}
	if err := r.verifyBootstrap(ctx); err != nil {
		return err
	}
	if err := r.credentials(ctx, true); err != nil {
		return err
	}
	for _, pl := range r.Plan.Placements {
		if r.Plan.Automatic && (pl.Role == deploy.Agent || pl.Role == deploy.Builder) {
			continue
		}
		p, ok := r.Config.Probes[pl.Role]
		if !ok {
			return fmt.Errorf("no runtime probe for %s", pl.Role)
		}
		p = r.expandProbe(p, pl)
		if p.RuntimeURL != "" {
			p.URL = p.RuntimeURL
		}
		if _, err := r.hostProbe(ctx, r.Plan, pl, p, false); err != nil {
			return err
		}
		if pl.Role == deploy.ControlPlane || pl.Role == deploy.Console || pl.Role == deploy.Registry {
			if _, err := r.hostProbe(ctx, r.Plan, pl, p, true); err != nil {
				return err
			}
		}
		// Observe reciprocal TCP reachability to every runtime dependency; declarative
		// RTT matrices are placement input, never proof of a working overlay.
		ports := map[deploy.Role]string{deploy.Database: "26257", deploy.ControlPlane: "9443", deploy.Registry: "5000"}
		dependencies := map[deploy.Role][]deploy.Role{deploy.ControlPlane: {deploy.Database, deploy.ControlPlane}, deploy.Console: {deploy.Database, deploy.ControlPlane}, deploy.Builder: {deploy.ControlPlane, deploy.Registry}, deploy.Agent: {deploy.ControlPlane, deploy.Registry}, deploy.Envoy: {deploy.ControlPlane}}
		for _, role := range dependencies[pl.Role] {
			for _, target := range r.Plan.Placements {
				if target.Role != role || target.Host == pl.Host {
					continue
				}
				h, _ := r.Plan.Installation.Host(target.Host)
				address := net.JoinHostPort(h.Network.Address, ports[role])
				if _, err := r.remote(ctx, r.Plan, pl, shell(operationsBinary(r.Plan))+" connect "+shell(address)+"\n"); err != nil {
					return fmt.Errorf("runtime dependency %s -> %s unavailable: %w", pl.Instance, target.Instance, err)
				}
			}
		}
	}
	if len(r.Plan.Installation.Endpoints) == 0 {
		return fmt.Errorf("production endpoints are required")
	}
	for _, endpoint := range r.Plan.Installation.Endpoints {
		probe, ok := r.Config.EndpointProbes[endpoint.Name]
		if !ok || !sameEndpoint(probe.URL, endpoint.URL) {
			return fmt.Errorf("endpoint %s lacks an exact HTTPS path inspection", endpoint.Name)
		}
		if _, err := probeHTTP(ctx, probe, false); err != nil {
			return err
		}
		if probe.AuthorizationURL != "" {
			if _, err := probeHTTP(ctx, probe, true); err != nil {
				return err
			}
		}
	}
	if err := r.consoleAdmin(ctx, "check-endpoints"); err != nil {
		return err
	}
	if err := r.verifyIngress(ctx); err != nil {
		return err
	}
	c, s, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	// Inspect actual pinned manifests through authenticated registry access.
	for _, ref := range r.Plan.Release.Images {
		if err := c.Images.Verify(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runner) verifyIngress(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var version string
	if err := db.QueryRowContext(ctx, `SELECT version FROM xds_publications WHERE id=TRUE`).Scan(&version); err != nil {
		return fmt.Errorf("no durable ingress publication: %w", err)
	}
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Envoy {
			continue
		}
		var applied string
		var age time.Time
		if err := db.QueryRowContext(ctx, `SELECT o.applied_version,o.updated_at FROM xds_node_observations o JOIN ingress_nodes n ON n.node_id=o.node_id WHERE n.node_id=$1 AND n.state='active'`, pl.Instance).Scan(&applied, &age); err != nil {
			return err
		}
		if version == "" || applied != version || time.Since(age) > 2*time.Minute {
			return fmt.Errorf("ingress %s has not freshly acknowledged the current publication", pl.Instance)
		}
	}
	return nil
}

// A path probe must stay on the declared HTTPS origin; a hostname prefix is not
// a service identity (example.com.attacker.invalid must never pass).
func sameEndpoint(probe, endpoint string) bool {
	p, pe := url.Parse(probe)
	e, ee := url.Parse(endpoint)
	return pe == nil && ee == nil && p.Scheme == "https" && e.Scheme == "https" &&
		p.User == nil && p.Host == e.Host && strings.HasPrefix(p.Path, e.Path)
}
