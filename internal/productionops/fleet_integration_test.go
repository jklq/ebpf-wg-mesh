//go:build integration && linux

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
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
	iniFile "gopkg.in/ini.v1"
)

// This opt-in fixture exercises the applied-plan engine and packaged reference
// operations through actual SSH, systemd, containerd, CockroachDB, Envoy and
// console processes. Docker is only the isolated infrastructure provider. It
// neither fabricates lifecycle receipts nor substitutes component readiness.
type nativeFleet struct {
	t                                           *testing.T
	ctx                                         context.Context
	root, ram, network, gateway, releaseDir, id string
	hosts                                       map[string]string
	installation                                deploy.Installation
	release                                     deploy.Release
	selection                                   Config
	storage                                     recovery.StorageConfig
	driver                                      *deploy.SSHDriver
	store                                       *deploy.Store
	mu                                          sync.RWMutex
	plan                                        deploy.Plan
	blocked                                     bool
	image                                       string
	serviceCA                                   certificateBundle
	allCA, public                               []byte
	fenceURL                                    string
	artifacts                                   *httptest.Server
	publisher                                   *exec.Cmd
}

func (f *nativeFleet) command(args ...string) []byte {
	f.t.Helper()
	cmd := exec.CommandContext(f.ctx, args[0], args[1:]...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fixture command %s failed: %v: %s", args[0], err, b)
	}
	return b
}
func (f *nativeFleet) docker(args ...string) []byte {
	return f.command(append([]string{"docker"}, args...)...)
}
func (f *nativeFleet) tlsServer(ca certificateBundle, handler http.Handler) *httptest.Server {
	f.t.Helper()
	leaf, err := certificate("fixture-service", &ca, []string{f.gateway, "*.example.invalid"})
	if err != nil {
		f.t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(leaf.Certificate, leaf.Key)
	if err != nil {
		f.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(f.gateway, "0"))
	if err != nil {
		f.t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = ln
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	f.t.Cleanup(server.Close)
	return server
}
func (f *nativeFleet) save(path string, v any) {
	f.t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(path, b)
}
func (f *nativeFleet) write(path string, b []byte) {
	f.t.Helper()
	if err := writePrivate(path, b); err != nil {
		f.t.Fatal(err)
	}
}

func newNativeFleet(t *testing.T, ctx context.Context) *nativeFleet {
	t.Helper()
	if os.Getenv("PRODUCTION_FLEET_INTEGRATION") != "1" {
		t.Skip("set PRODUCTION_FLEET_INTEGRATION=1 and provide the packaged release and isolated host image")
	}
	releaseDir := os.Getenv("PRODUCTION_RELEASE_DIRECTORY")
	image := os.Getenv("PRODUCTION_HOST_IMAGE")
	if image == "" {
		image = "platform-operations-fixture/host:r43"
	}
	if releaseDir == "" {
		t.Fatal("PRODUCTION_RELEASE_DIRECTORY is required")
	}
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	id := "native" + hex.EncodeToString(random)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".platform-fleet-")
	if err != nil {
		t.Fatal(err)
	}
	ram, err := os.MkdirTemp("/dev/shm", "platform-fleet-")
	if err != nil {
		t.Fatal(err)
	}
	f := &nativeFleet{t: t, ctx: ctx, root: root, ram: ram, id: id, network: id, releaseDir: releaseDir, hosts: map[string]string{}, driver: deploy.NewSSHDriver()}
	t.Cleanup(func() {
		for _, name := range f.hosts {
			exec.Command("docker", "rm", "--force", name).Run()
		}
		// Host-admin services own private files as root. Restore ownership only
		// within this fixture's private mount before removing its on-disk data.
		exec.Command("docker", "run", "--rm", "--mount", "type=bind,src="+ram+",dst=/owned", image, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/owned").Run()
		exec.Command("docker", "network", "rm", f.network).Run()
		os.RemoveAll(root)
		os.RemoveAll(ram)
	})
	// Random ULA isolates the mesh underlay without changing host DNS/routes.
	subnet := fmt.Sprintf("fd%02x:%02x%02x:%02x%02x:%02x00::/64", random[0], random[1], random[2], random[3], random[4], random[5])
	f.docker("network", "create", "--ipv6", "--subnet", subnet, "--label", "platform.native-fixture="+id, f.network)
	var networks []struct {
		IPAM struct{ Config []struct{ Gateway string } }
	}
	if err = json.Unmarshal(f.docker("network", "inspect", f.network), &networks); err != nil {
		t.Fatal(err)
	}
	for _, entry := range networks[0].IPAM.Config {
		if ip := net.ParseIP(entry.Gateway); ip != nil && ip.To4() != nil {
			f.gateway = entry.Gateway
		}
	}
	if f.gateway == "" {
		t.Fatal("fixture network has no actual IPv4 gateway")
	}
	f.command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", root+"/ssh")
	public := mustRead(t, root+"/ssh.pub")
	ca, err := certificate("fleet-service-ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.write(root+"/services-ca.crt", ca.Certificate)
	wildcard, err := certificate("wildcard", &ca, []string{"*.example.invalid", f.gateway})
	if err != nil {
		t.Fatal(err)
	}
	f.write(root+"/wildcard.crt", wildcard.Certificate)
	f.write(root+"/wildcard.key", wildcard.Key)
	f.storage = nativeObjectStoreAt(t, ctx, f.gateway, true)
	active := nativeObjectStoreAt(t, ctx, f.gateway, false)
	// Explicit private files, shared only by this isolated fixture and copied by
	// the same SSH driver as production. The selected stores have real TLS roots.
	for name, path := range map[string]string{"recovery-writer": f.storage.CredentialsFile, "recovery-ca.crt": f.storage.CAFile, "active-writer": active.CredentialsFile, "active-ca.crt": active.CAFile} {
		f.write(root+"/"+name, mustRead(t, path))
	}
	f.storage.CredentialsFile = root + "/recovery-writer"
	f.storage.CAFile = root + "/recovery-ca.crt"
	active.CredentialsFile = root + "/active-writer"
	active.CAFile = root + "/active-ca.crt"
	// This configured external gateway forwards only to live component processes.
	// A failed write is never replayed; selection occurs before a request is sent.
	backend := func(role deploy.Role, port, path string) *httptest.Server {
		return f.tlsServer(ca, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			f.mu.RLock()
			plan, blocked := f.plan, f.blocked
			f.mu.RUnlock()
			if blocked && role == deploy.Console && request.URL.Path == "/readyz" {
				http.Error(w, "injected external endpoint outage", 503)
				return
			}
			for _, pl := range plan.Placements {
				if pl.Role != role {
					continue
				}
				host, _ := plan.Installation.Host(pl.Host)
				address := net.JoinHostPort(host.Network.Address, port)
				conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
				if err != nil {
					continue
				}
				conn.Close()
				scheme := "http"
				if role == deploy.Envoy || port == "9444" {
					scheme = "https"
				}
				target := &url.URL{Scheme: scheme, Host: address}
				proxy := httputil.NewSingleHostReverseProxy(target)
				if scheme == "https" {
					roots, serverName := ca.Certificate, "verification.example.invalid"
					if role == deploy.ControlPlane {
						var err error
						roots, err = exec.CommandContext(f.ctx, "docker", "exec", f.hosts[pl.Host], "cat", cfgDir(plan, pl)+"/ca.crt").Output()
						if err != nil {
							http.Error(w, "upstream trust unavailable", 503)
							return
						}
						serverName = "core.example.invalid"
					}
					transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: fixtureRoots(roots), ServerName: serverName, MinVersion: tls.VersionTLS12}}
					defer transport.CloseIdleConnections()
					proxy.Transport = transport
				}
				if path != "" {
					request.URL.Path = path
				}
				proxy.ServeHTTP(w, request)
				return
			}
			http.Error(w, "no live backend", http.StatusServiceUnavailable)
		}))
	}
	console := backend(deploy.Console, "3000", "")
	registry := backend(deploy.Registry, "5000", "")
	envoy := backend(deploy.Envoy, "443", "")
	realm := backend(deploy.ControlPlane, "9444", "")
	monitor := f.tlsServer(ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var report map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&report); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.save(f.root+"/monitor-latest.json", report)
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	artifacts := f.tlsServer(ca, http.FileServer(http.Dir(releaseDir)))
	f.artifacts = artifacts
	f.release, err = deploy.Load[deploy.Release](releaseDir + "/release.json")
	if err != nil {
		t.Fatal(err)
	}
	for role, prog := range f.release.Programs {
		for arch, a := range prog.Artifacts {
			a.URL = artifacts.URL + "/" + string(role) + "-" + arch
			prog.Artifacts[arch] = a
		}
		f.release.Programs[role] = prog
	}
	for name, arches := range f.release.Tools {
		for arch, a := range arches {
			a.URL = artifacts.URL + "/" + name + "-" + arch
			arches[arch] = a
		}
	}
	// The builder publisher is independently selected and loss can be injected
	// after its complete OCI closure has reached recovery storage.
	f.startImagePublisher(ca)
	i, err := deploy.Load[deploy.Installation]("../../infra/production/examples/providers-redundant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	i.OperationsInputs = nil
	i.ID = id
	i.Release = f.release.ID
	i.OperationsConfig = root + "/operations.json"
	i.Providers = map[string]deploy.Provider{"imported": {Kind: "linux"}}
	i.Secrets = map[string]deploy.SecretRef{"ssh": {File: root + "/ssh"}, "backup-writer": {File: f.storage.CredentialsFile}, "recovery-key": {File: root + "/recovery.key"}, "recovery": {File: f.storage.CredentialsFile}}
	i.Recovery.Inventory = "/var/lib/ebpf-wg-mesh/" + id + "/fleet-inventory.json"
	i.Backup = deploy.Backup{Target: "s3://" + f.storage.Bucket + "/" + f.storage.Prefix, Credentials: []string{"backup-writer"}, RecoveryKey: "recovery-key", Account: f.storage.Account, PrimaryAccount: f.storage.PrimaryAccount, FailureDomain: f.storage.FailureDomain, Monitor: monitor.URL}
	i.Endpoints = []deploy.Endpoint{{Name: "console", Role: deploy.Console, URL: console.URL, Hosts: []string{"a", "b", "c"}}, {Name: "registry", Role: deploy.Registry, URL: registry.URL, Hosts: []string{"a", "b", "c"}}, {Name: "envoy", Role: deploy.Envoy, URL: envoy.URL, Hosts: []string{"a", "b", "c"}}}
	allCA := append(append(append([]byte{}, ca.Certificate...), mustRead(t, f.storage.CAFile)...), mustRead(t, active.CAFile)...)
	f.image, f.serviceCA, f.allCA, f.public = image, ca, allCA, public
	for n := range i.Hosts {
		i.Hosts[n] = f.prepareHost(i.Hosts[n])
	}
	i.Recovery.Hosts = nil // Replacement hosts are added by an applied recovery plan.
	// Preserve the non-purchasing independent-host recovery prerequisite.
	rescue := i.Hosts[0]
	rescue.ID = "recovery"
	rescue.Binding.ServerID = id + "-recovery"
	rescue.FailureDomain = "fixture/recovery"
	i.Recovery.Hosts = []deploy.Host{rescue}
	builder := i.Components[deploy.Builder]
	builder.Capabilities = []string{"containerd", "cgroup-v2"}
	builder.Resources.MemoryMiB = 2048
	i.Components[deploy.Builder] = builder
	core := i.Components[deploy.ControlPlane]
	core.Env = map[string]string{"CONTROLPLANE_STATE_DIR": "{stateDir}", "CONTROLPLANE_SECRET_KEYS_KEYRING": "{configDir}/keyring.json"}
	i.Components[deploy.ControlPlane] = core
	f.write(root+"/registry-auth.json", []byte(`{"auths":{}}`))
	f.write(root+"/recovery.key", []byte(strings.Repeat("R", 32)))
	rec := recovery.Config{Storage: f.storage, RecoveryKeyFile: root + "/recovery.key", Installation: id, Release: f.release.ID, DatabaseURLFile: root + "/database-url", KeyringFile: root + "/keyring.json", ConsoleSchema: "dashboard", BackupConnection: "external://native_recovery", BackupPrefix: f.storage.Prefix + "/database", Images: recovery.Images{Binary: "/opt/ebpf-wg-mesh/" + id + "/" + f.release.ID + "/tools/skopeo", AuthFile: root + "/registry-auth.json", CertificateDirectory: root + "/image-ca"}}
	f.save(root+"/recovery.json", rec)
	credentials, err := iniFile.Load(mustRead(t, f.storage.CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	section := credentials.Section(f.storage.Profile)
	uri := url.URL{Scheme: "s3", Host: f.storage.Bucket, Path: "/" + rec.BackupPrefix}
	uri.RawQuery = url.Values{"AWS_ACCESS_KEY_ID": {section.Key("aws_access_key_id").String()}, "AWS_SECRET_ACCESS_KEY": {section.Key("aws_secret_access_key").String()}, "AWS_REGION": {f.storage.Region}, "AWS_ENDPOINT": {f.storage.Endpoint}}.Encode()
	f.write(root+"/backup-uri", []byte(uri.String()))
	// The active archive driver and native distribution use actual external S3.
	// Same-machine storage measures algorithm behavior, not physical independence.
	activeCreds, err := iniFile.Load(mustRead(t, active.CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	f.save(root+"/source-credentials.json", map[string]string{"accessKeyId": activeCreds.Section(active.Profile).Key("aws_access_key_id").String(), "secretAccessKey": activeCreds.Section(active.Profile).Key("aws_secret_access_key").String()})
	f.save(root+"/source.json", config.SourceArchiveConfig{Provider: "s3", S3: config.SourceArchiveS3Config{CAFile: active.CAFile, Endpoint: active.Endpoint, Region: active.Region, Bucket: active.Bucket, Prefix: "archives", CredentialsFile: root + "/source-credentials.json", ServerSideEncryption: "AES256"}})
	f.selection = Config{Version: 1, StateDirectory: "/var/lib/ebpf-wg-mesh/" + id + "/operations", RecoveryConfig: root + "/recovery.json", Database: DatabaseConfig{Binary: "/opt/ebpf-wg-mesh/" + id + "/" + f.release.ID + "/tools/cockroachdb", CertificateDirectory: "/var/lib/ebpf-wg-mesh/" + id + "/database-pki", URLFile: root + "/database-url", Name: "platform", BackupURIFile: root + "/backup-uri"}, Console: ConsoleConfig{AdminBinary: "/opt/ebpf-wg-mesh/" + id + "/" + f.release.ID + "/tools/console-admin", Schema: "dashboard", TokenKeyFile: root + "/console.key"}, PlatformDomain: "example.invalid", InternalServerName: "core.example.invalid", WildcardCertificate: root + "/wildcard.crt", WildcardKey: root + "/wildcard.key", RegistryRealm: realm.URL + "/token", RegistryService: strings.TrimPrefix(registry.URL, "https://"), SourceConfig: root + "/source.json", Storage: map[string]StoreConfig{"registry": {Kind: "s3", S3: active}, "archives": {Kind: "s3", S3: active}}, Probes: map[deploy.Role]Probe{}, EndpointProbes: map[string]Probe{}, BuilderSandbox: config.BuilderSandboxConfig{Snapshotter: "native", CNIConfDir: "/etc/cni/build-sandbox.d", CNINetwork: "build-sandbox"}}
	var reference Config
	if err = json.Unmarshal(mustRead(t, "../../infra/production/operations/config.example.json"), &reference); err != nil {
		t.Fatal(err)
	}
	f.selection.Probes = reference.Probes
	p := f.selection.Probes[deploy.Console]
	p.AuthorizationURL = console.URL + "/sessionz"
	p.CAFile = root + "/services-ca.crt"
	f.selection.Probes[deploy.Console] = p
	p = f.selection.Probes[deploy.Registry]
	p.AuthorizationURL = registry.URL + "/v2/"
	p.CAFile = root + "/services-ca.crt"
	f.selection.Probes[deploy.Registry] = p
	f.selection.EndpointProbes = map[string]Probe{"console": {URL: console.URL + "/readyz", CAFile: root + "/services-ca.crt", Status: 200, BodyContains: `"ready"`, AuthorizationURL: console.URL + "/sessionz", UnauthorizedStatus: 401}, "registry": {URL: registry.URL + "/v2/", CAFile: root + "/services-ca.crt", Status: 401}, "envoy": {URL: envoy.URL + "/unassigned", CAFile: root + "/services-ca.crt", Status: 404}}
	f.installFences()
	f.save(i.OperationsConfig, f.selection)
	for _, name := range []string{"operations.json", "recovery.json", "recovery.key", "recovery-writer", "recovery-ca.crt", "active-writer", "active-ca.crt", "services-ca.crt", "wildcard.crt", "wildcard.key", "backup-uri", "registry-auth.json", "source.json", "source-credentials.json", "image-ca/ca.crt", "fence-credentials.json"} {
		i.OperationsInputs = append(i.OperationsInputs, root+"/"+name)
	}
	f.installation = i
	f.store, err = deploy.OpenState(root+"/installer.enc", []byte(strings.Repeat("S", 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	return f
}

func (f *nativeFleet) build(automatic bool) deploy.Plan {
	f.t.Helper()
	state, err := f.store.Read()
	if err != nil {
		f.t.Fatal(err)
	}
	inv, err := f.driver.Inventory(f.ctx, f.installation, f.release, state)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := deploy.BuildPlan(f.installation, f.release, state, inv, automatic, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	if len(p.Unmet) != 0 {
		f.t.Fatalf("valid native installation cannot place: %+v; observed: %+v", p.Unmet, inv)
	}
	f.mu.Lock()
	f.plan = p
	f.mu.Unlock()
	// Real content-verified local caches avoid storing the identical executable
	// N times on the root filesystem. Stage still verifies every selected digest.
	for _, h := range p.Installation.Hosts {
		hostRoot := f.ram + "/" + h.ID + "/opt/" + p.Installation.ID + "/" + p.Release.ID
		for name, arches := range p.Release.Tools {
			a := arches[h.Architecture]
			f.cache(hostRoot+"/tools/"+name, name+"-"+h.Architecture, a.SHA256)
		}
	}
	for _, pl := range p.Placements {
		h, _ := p.Installation.Host(pl.Host)
		f.cache(f.ram+"/"+h.ID+"/opt/"+p.Installation.ID+"/"+p.Release.ID+"/"+pl.Instance+"/program", string(pl.Role)+"-"+h.Architecture, p.Release.Programs[pl.Role].Artifacts[h.Architecture].SHA256)
	}
	return p
}
func (f *nativeFleet) cache(target, source, hash string) {
	f.t.Helper()
	actual, _, err := recovery.FileDigest(f.releaseDir + "/" + source)
	if err != nil || actual != "sha256:"+hash {
		f.t.Fatal("fixture artifact disagrees with release", source, err)
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err = os.Link(f.releaseDir+"/"+source, target); err != nil && !os.IsExist(err) {
		f.t.Fatal(err)
	}
}
func (f *nativeFleet) apply(p deploy.Plan) {
	f.t.Helper()
	engine := deploy.Engine{Store: f.store, Driver: f.driver}
	var err error
	lastCompleted, stalled := -1, 0
	for attempts := 0; attempts < 25; attempts++ {
		err = engine.Apply(f.ctx, p)
		if err == nil {
			f.t.Log("applied", p.ID)
			return
		}
		state, _ := f.store.Read()
		if state.Progress == nil && strings.Contains(err.Error(), "materially stale plan: observed inventory changed") {
			// No effect has been admitted. This isolated operator applies a new
			// concrete plan from actual inventory, including native range splits.
			p = f.build(p.Automatic)
			lastCompleted, stalled = -1, 0
			continue
		}
		completed := 0
		if state.Progress != nil {
			completed = len(state.Progress.Completed)
		}
		f.t.Logf("native apply attempt %d, completed=%d: %v", attempts+1, completed, err)
		if completed == lastCompleted {
			stalled++
		} else {
			stalled = 0
			lastCompleted = completed
		}
		if stalled >= 2 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	f.diagnose(p)
	f.t.Fatal(err)
}
func (f *nativeFleet) diagnose(p deploy.Plan) {
	state, _ := f.store.Read()
	if state.Progress != nil {
		for _, op := range p.Operations {
			if !state.Progress.Started[op.ID] {
				continue
			}
			if _, done := state.Progress.Completed[op.ID]; done {
				continue
			}
			if op.Hook != "" && op.Kind == "hook" {
				args := []string{"exec", "-e", "PLATFORM_PLAN=/etc/ebpf-wg-mesh/" + p.Installation.ID + "/plan.json", "-e", "PLATFORM_OPERATIONS_CONFIG=" + p.Installation.OperationsConfig}
				if p.Recovery && state.LastRestore != nil {
					args = append(args, "-e", "PLATFORM_RECOVERY_OPERATION=/etc/ebpf-wg-mesh/"+p.Installation.ID+"/recovery.json", "-e", "PLATFORM_BACKUP="+state.LastRestore.Backup, "-e", "PLATFORM_DATA_LOSS_CUTOFF="+state.LastRestore.DataLossCutoff.Format(time.RFC3339Nano))
				}
				args = append(args, f.hosts[op.Host], operationsBinary(p), "verify", op.Hook)
				cmd := exec.CommandContext(f.ctx, "docker", args...)
				b, _ := cmd.CombinedOutput()
				f.t.Logf("native %s verification: %s", op.Hook, b)
				if op.Hook == "backup-schedule" {
					path := filepath.Join(f.selection.StateDirectory, p.ID, "recovery.json")
					data, err := exec.CommandContext(f.ctx, "docker", "exec", f.hosts[op.Host], "cat", path).Output()
					var selected recovery.Config
					if err == nil && json.Unmarshal(data, &selected) == nil {
						f.t.Logf("native effective recovery release=%s, image helper=%s", selected.Release, selected.Images.Binary)
					}
				}
				message := strings.TrimSpace(string(b))
				if _, diagnostic, found := strings.Cut(message, "private diagnostic: "); found && strings.HasPrefix(diagnostic, "/var/log/ebpf-wg-mesh/"+f.id+"/") {
					if detail, err := exec.CommandContext(f.ctx, "docker", "exec", f.hosts[op.Host], "cat", diagnostic).Output(); err == nil {
						f.t.Logf("native private verification error: %s", detail)
					}
				}
			}
		}
	}

	for _, h := range p.Installation.Hosts {
		for _, pl := range p.Placements {
			if pl.Host != h.ID {
				continue
			}
			cmd := exec.CommandContext(f.ctx, "docker", "exec", f.hosts[h.ID], "journalctl", "--no-pager", "-u", unit(p, pl), "-n", "8")
			if b, err := cmd.CombinedOutput(); err == nil {
				f.t.Logf("%s: %s", pl.Instance, b)
			}
		}
	}
	if os.Getenv("PRODUCTION_FLEET_DIAGNOSTIC_HOLD") == "1" {
		f.t.Log("native failure inspection window: 60 seconds before fixture cleanup")
		select {
		case <-f.ctx.Done():
		case <-time.After(time.Minute):
		}
	}
}

func (f *nativeFleet) freshInstallation() {
	f.t.Helper()
	p := f.build(false)
	started := time.Now()
	f.apply(p)
	f.t.Log("native fresh fleet elapsed", time.Since(started))
	state, err := f.store.Read()
	if err != nil || state.LastBackup.Backup == "" {
		f.t.Fatal("fresh native fleet lacks verified complete recovery point", err)
	}
	if _, err = os.Stat(f.root + "/monitor-latest.json"); err != nil {
		f.t.Fatal("actual independent monitor did not receive completion", err)
	}
}

func TestNativeSSHSystemdProductionFleet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	f := newNativeFleet(t, ctx)
	f.freshInstallation()
	f.failedUpgrade(t)
	f.hostFailure(t)
}

// Isolate recovery faults without repeating the upgrade/failover scenarios.
// The fixture still installs the real complete platform and creates native
// recovery points; it never preloads an assumed successful deployment state.
func TestNativeSSHSystemdRecoveryFleet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	f := newNativeFleet(t, ctx)
	f.freshInstallation()
	f.restoreFleet(t, false)
	// Inspect a new protected snapshot after recovery and independent renewal.
	f.completePoint()
}

func TestNativeSSHSystemdSiteLossFleet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	f := newNativeFleet(t, ctx)
	f.freshInstallation()
	f.restoreFleet(t, true)
}

func fixtureRoots(pem []byte) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	return pool
}

func (f *nativeFleet) startImagePublisher(ca certificateBundle) {
	f.t.Helper()
	binary := os.Getenv("REGISTRY_BINARY")
	if binary == "" {
		binary = "/tmp/platform-native-inputs/registry"
	}
	layout := os.Getenv("PRODUCTION_TOOLCHAIN_OCI")
	if layout == "" {
		layout = "/dev/shm/platform-toolchain-oci"
	}
	if _, err := os.Stat(layout + "/index.json"); err != nil {
		f.t.Fatal("provide the verified native toolchain OCI layout", err)
	}
	address := freeAddress(f.t, f.gateway)
	leaf, err := certificate("builder-publisher", &ca, []string{f.gateway})
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(f.root+"/publisher.crt", leaf.Certificate)
	f.write(f.root+"/publisher.key", leaf.Key)
	f.write(f.root+"/image-ca/ca.crt", ca.Certificate)
	f.write(f.root+"/publisher.yaml", []byte("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: "+f.ram+"/publisher\nhttp:\n  addr: "+address+"\n  tls:\n    certificate: "+f.root+"/publisher.crt\n    key: "+f.root+"/publisher.key\n"))
	log, err := os.Create(f.root + "/publisher.log")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { log.Close() })
	process := exec.CommandContext(f.ctx, binary, "serve", f.root+"/publisher.yaml")
	f.publisher = process
	process.Stdout = log
	process.Stderr = log
	isolateChild(process)
	if err = process.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { process.Process.Kill(); process.Wait() })
	for attempt := 0; attempt < 100; attempt++ {
		if _, err = probeHTTP(f.ctx, Probe{URL: "https://" + address + "/v2/", CAFile: f.root + "/services-ca.crt", Status: 200}, false); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ref := f.release.Images["builder-sandbox"]
	root := ref[strings.LastIndex(ref, "@")+1:]
	f.command(f.releaseDir+"/skopeo-amd64", "--insecure-policy", "copy", "--all", "--preserve-digests", "--dest-cert-dir", f.root+"/image-ca", "oci:"+layout, "docker://"+address+"/toolchain:native")
	f.release.Images["builder-sandbox"] = address + "/toolchain@" + root
}
