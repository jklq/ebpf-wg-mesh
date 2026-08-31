package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/testutil"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	vmUserID              = "vm-user"
	vmUserAssertionSecret = "vm-user-assertion-secret-at-least-32-bytes"
	// Default controlplane dashboard ServiceCallerID; must match minted client cert CN.
	vmDashboardCallerID = "dashboard"
)

// Per-agent bootstrap tokens must be unique: controlplane rejects a token bound to more than one agent.
var vmAgentBootstrapTokens = map[string]string{
	"agent-a": "vm-bootstrap-token-agent-a",
	"agent-b": "vm-bootstrap-token-agent-b",
}

type hostInfo struct {
	Role        string `json:"role"`
	Name        string `json:"name"`
	PublicIPv4  string `json:"public_ipv4"`
	PublicIPv6  string `json:"public_ipv6"`
	PrivateIPv4 string `json:"private_ipv4"`
}

type clientIdentity struct {
	CAPEMB64   string `json:"ca_pem_b64"`
	CertPEMB64 string `json:"cert_pem_b64"`
	KeyPEMB64  string `json:"key_pem_b64"`
}

type summary struct {
	RunID        string              `json:"run_id"`
	Scenario     string              `json:"scenario"`
	StartedAt    time.Time           `json:"started_at"`
	CompletedAt  time.Time           `json:"completed_at"`
	ArtifactsDir string              `json:"artifacts_dir"`
	Hosts        map[string]hostInfo `json:"hosts"`
}

func main() {
	// failf panics so deferred cleanup (tofu destroy) still runs before process exit.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "%v\n", r)
			os.Exit(1)
		}
	}()

	var (
		runID     = flag.String("run-id", fmt.Sprintf("vm-%d", time.Now().Unix()), "test run id")
		scenario  = flag.String("scenario", "service-rollout", "scenario name")
		tofuDir   = flag.String("tofu-dir", "infra/test-vm/tofu", "tofu configuration directory")
		artifacts = flag.String("artifacts-dir", "", "artifact directory")
		tofuBin   = flag.String("tofu-bin", "tofu", "tofu binary")
		location  = flag.String("location", "nbg1", "Hetzner location")
		image     = flag.String("image", "ubuntu-24.04", "Hetzner image name")
		cpType    = flag.String("controlplane-type", "", "Hetzner controlplane server type (default: cheapest orderable shared x86 type in the selected location)")
		agentType = flag.String("agent-type", "", "Hetzner agent server type (default: cheapest orderable shared x86 type in the selected location)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	startedAt := time.Now().UTC()

	repoRoot, err := os.Getwd()
	if err != nil {
		failf("getwd: %v", err)
	}
	// Optional local overrides (e.g. HCLOUD_TOKEN), including a 1Password-mounted .env FIFO.
	// Existing process env wins; missing .env is fine.
	if n, err := localteststack.LoadDotEnvFile(filepath.Join(repoRoot, ".env")); err != nil {
		failf("load .env: %v", err)
	} else if n > 0 {
		infof("loaded %d variable(s) from .env", n)
	}

	token := strings.TrimSpace(os.Getenv("HCLOUD_TOKEN"))
	if token == "" {
		failf("missing HCLOUD_TOKEN (export it or set it in repo-root .env)")
	}
	client := hcloud.NewClient(hcloud.WithToken(token))
	if strings.TrimSpace(*cpType) == "" || strings.TrimSpace(*agentType) == "" {
		selectedType, err := selectCheapestServerType(ctx, client, *location)
		if err != nil {
			failf("select server type for %s: %v", *location, err)
		}
		if strings.TrimSpace(*cpType) == "" {
			*cpType = selectedType
		}
		if strings.TrimSpace(*agentType) == "" {
			*agentType = selectedType
		}
		infof("selected server type for %s: %s", *location, selectedType)
	}

	artifactRoot := *artifacts
	if artifactRoot == "" {
		artifactRoot = filepath.Join(repoRoot, "artifacts", "e2e-vm", *runID)
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		failf("mkdir artifacts: %v", err)
	}

	binDir := filepath.Join(artifactRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		failf("mkdir bin dir: %v", err)
	}
	credentialDir, err := os.MkdirTemp("", "ebpf-wg-mesh-testvm-credentials-")
	if err != nil {
		failf("create temporary credential dir: %v", err)
	}
	defer os.RemoveAll(credentialDir)
	if err := os.Chmod(credentialDir, 0o700); err != nil {
		failf("secure temporary credential dir: %v", err)
	}
	sshKeyPath := filepath.Join(credentialDir, "runner_ed25519")
	if err := generateSSHKey(ctx, sshKeyPath); err != nil {
		failf("generate ssh key: %v", err)
	}

	binaries := map[string]string{
		"controlplane":         filepath.Join(binDir, "controlplane"),
		"agent":                filepath.Join(binDir, "agent"),
		"internal-client-cert": filepath.Join(binDir, "internal-client-cert"),
	}
	infof("building Linux binaries")
	if err := buildBinaries(ctx, repoRoot, binaries); err != nil {
		failf("build binaries: %v", err)
	}

	tfDataDir := filepath.Join(artifactRoot, "tofu-data")
	if err := os.MkdirAll(tfDataDir, 0o755); err != nil {
		failf("mkdir tofu data dir: %v", err)
	}
	varsPath := filepath.Join(artifactRoot, "tofu.auto.tfvars.json")
	if err := writeJSON(varsPath, map[string]any{
		"run_id":                   *runID,
		"ssh_public_key_path":      sshKeyPath + ".pub",
		"location":                 *location,
		"image":                    *image,
		"controlplane_server_type": *cpType,
		"agent_server_type":        *agentType,
	}); err != nil {
		failf("write tofu vars: %v", err)
	}

	tofuEnv := append(os.Environ(), "TF_DATA_DIR="+tfDataDir, "TF_VAR_hcloud_token="+token)
	infof("running tofu init")
	if err := runCommand(ctx, repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "init", "-input=false"); err != nil {
		failf("tofu init: %v", err)
	}
	destroyed := false
	defer func() {
		if destroyed {
			return
		}
		infof("destroying vm environment")
		_ = runCommand(context.Background(), repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "destroy", "-auto-approve", "-input=false", "-var-file="+varsPath)
	}()
	infof("applying vm environment in %s", *location)
	if err := runCommand(ctx, repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "apply", "-auto-approve", "-input=false", "-var-file="+varsPath); err != nil {
		failf("tofu apply: %v", err)
	}

	infof("reading provisioned host outputs")
	hosts, err := readHostsOutput(ctx, repoRoot, tofuEnv, *tofuBin, *tofuDir)
	if err != nil {
		failf("read tofu outputs: %v", err)
	}

	infof("waiting for Hetzner to report %d running servers", len(hosts))
	if err := waitForServers(ctx, client, *runID, len(hosts)); err != nil {
		failf("wait for Hetzner servers: %v", err)
	}

	for _, host := range hosts {
		infof("waiting for ssh on %s (%s)", host.Name, host.PublicIPv4)
		if err := waitForSSH(ctx, sshKeyPath, host.PublicIPv4); err != nil {
			failf("wait for ssh on %s: %v", host.Name, err)
		}
		infof("waiting for cloud-init on %s", host.Name)
		if err := waitForCloudInit(ctx, sshKeyPath, host.PublicIPv4); err != nil {
			failf("wait for cloud-init on %s: %v", host.Name, err)
		}
		infof("preparing host %s", host.Name)
		if _, err := runRemoteCommand(ctx, sshKeyPath, host.PublicIPv4, "mkdir -p /opt/ebpf-wg-mesh"); err != nil {
			failf("prepare host %s: %v", host.Name, err)
		}
	}

	controlplane := hosts["controlplane"]
	infof("copying controlplane binaries to %s", controlplane.Name)
	if err := copyFile(ctx, sshKeyPath, binaries["controlplane"], controlplane.PublicIPv4, "/opt/ebpf-wg-mesh/controlplane"); err != nil {
		failf("copy controlplane binary: %v", err)
	}
	if err := copyFile(ctx, sshKeyPath, binaries["internal-client-cert"], controlplane.PublicIPv4, "/opt/ebpf-wg-mesh/internal-client-cert"); err != nil {
		failf("copy internal-client-cert binary: %v", err)
	}
	infof("installing controlplane on %s", controlplane.Name)
	bootstrapBindings := make([]string, 0, len(vmAgentBootstrapTokens))
	for agentID, token := range vmAgentBootstrapTokens {
		bootstrapBindings = append(bootstrapBindings, agentID+"="+token)
	}
	// Stable order for systemd unit / logs.
	sort.Strings(bootstrapBindings)
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-controlplane.sh"), map[string]string{
		"PUBLIC_ADDR":            "platform.local",
		"AGENT_BOOTSTRAP_TOKENS": strings.Join(bootstrapBindings, ","),
		"USER_ASSERTION_SECRET":  vmUserAssertionSecret,
	}); err != nil {
		failf("install controlplane: %v", err)
	}
	infof("waiting for controlplane readiness on %s", controlplane.Name)
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet ebpf-wg-mesh-controlplane && test -f /var/lib/ebpf-wg-mesh/controlplane/pki/ca.crt && ss -ltn '( sport = :9443 )' | grep -q LISTEN"); err != nil {
		failf("wait for controlplane readiness: %v", err)
	}

	caPath := filepath.Join(artifactRoot, "controlplane-ca.crt")
	infof("fetching controlplane ca certificate")
	if err := copyFromRemote(ctx, sshKeyPath, controlplane.PublicIPv4, "/var/lib/ebpf-wg-mesh/controlplane/pki/ca.crt", caPath); err != nil {
		failf("fetch controlplane ca: %v", err)
	}

	for key, host := range hosts {
		if key == "controlplane" {
			continue
		}
		bootstrapToken, ok := vmAgentBootstrapTokens[key]
		if !ok {
			failf("no bootstrap token configured for agent key %q", key)
		}
		infof("copying agent binary to %s", host.Name)
		if err := copyFile(ctx, sshKeyPath, binaries["agent"], host.PublicIPv4, "/opt/ebpf-wg-mesh/agent"); err != nil {
			failf("copy agent binary to %s: %v", host.Name, err)
		}
		if err := copyFile(ctx, sshKeyPath, caPath, host.PublicIPv4, "/opt/ebpf-wg-mesh/controlplane-ca.crt"); err != nil {
			failf("copy ca to %s: %v", host.Name, err)
		}
		infof("installing agent on %s (node-id=%s)", host.Name, key)
		// NODE_ID must match the agent_id in AGENT_BOOTSTRAP_TOKENS and the hosts map key used for lookups.
		// Reach the control plane over public IPv4 so enrollment does not depend on private-network
		// route readiness. Mesh peer endpoints still use each agent's advertised public IPv6.
		if err := runRemoteScript(ctx, sshKeyPath, host.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-agent.sh"), map[string]string{
			"NODE_ID":              key,
			"NODE_NAME":            host.Name,
			"ADVERTISE_ADDR":       trimCIDR(host.PublicIPv6),
			"CONTROLPLANE_ADDRESS": controlplane.PublicIPv4 + ":9443",
			"BOOTSTRAP_TOKEN":      bootstrapToken,
		}); err != nil {
			failf("install agent on %s: %v", host.Name, err)
		}
		if err := waitForRemoteCommand(ctx, sshKeyPath, host.PublicIPv4, "systemctl is-active --quiet ebpf-wg-mesh-agent"); err != nil {
			dumpRemoteDiagnostics(ctx, sshKeyPath, host)
			failf("wait for agent service on %s: %v", host.Name, err)
		}
	}

	identity, err := fetchClientIdentity(ctx, sshKeyPath, controlplane.PublicIPv4)
	if err != nil {
		failf("fetch dashboard client identity: %v", err)
	}
	infof("running service rollout scenario against %s", controlplane.Name)
	if err := runServiceRolloutScenario(ctx, controlplane.PublicIPv4+":9443", identity, sshKeyPath, hosts); err != nil {
		infof("scenario failed; collecting diagnostics into %s", artifactRoot)
		_ = collectArtifacts(ctx, repoRoot, artifactRoot, sshKeyPath, hosts)
		failf("run service rollout scenario: %v", err)
	}

	infof("collecting artifacts into %s", artifactRoot)
	if err := collectArtifacts(ctx, repoRoot, artifactRoot, sshKeyPath, hosts); err != nil {
		failf("collect artifacts: %v", err)
	}

	if err := writeJSON(filepath.Join(artifactRoot, "summary.json"), summary{
		RunID:        *runID,
		Scenario:     *scenario,
		StartedAt:    startedAt,
		CompletedAt:  time.Now().UTC(),
		ArtifactsDir: artifactRoot,
		Hosts:        hosts,
	}); err != nil {
		failf("write summary: %v", err)
	}

	infof("destroying vm environment")
	if err := runCommand(ctx, repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "destroy", "-auto-approve", "-input=false", "-var-file="+varsPath); err != nil {
		failf("tofu destroy: %v", err)
	}
	destroyed = true
	infof("vm test run completed successfully")
}

func buildBinaries(ctx context.Context, repoRoot string, binaries map[string]string) error {
	builds := map[string]string{
		"./cmd/controlplane":         binaries["controlplane"],
		"./cmd/agent":                binaries["agent"],
		"./cmd/internal-client-cert": binaries["internal-client-cert"],
	}
	for pkg, output := range builds {
		if err := runCommand(ctx, repoRoot, append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"), "go", "build", "-o", output, pkg); err != nil {
			return err
		}
	}
	return nil
}

func generateSSHKey(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return runCommand(ctx, ".", os.Environ(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
}

func readHostsOutput(ctx context.Context, repoRoot string, env []string, tofuBin, tofuDir string) (map[string]hostInfo, error) {
	cmd := exec.CommandContext(ctx, tofuBin, "-chdir="+tofuDir, "output", "-json", "hosts")
	cmd.Dir = repoRoot
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var hosts map[string]hostInfo
	if err := json.Unmarshal(output, &hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

func waitForServers(ctx context.Context, client *hcloud.Client, runID string, expected int) error {
	selector := "ebpf-wg-mesh.run-id=" + runID
	attempt := 0
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		servers, _, err := client.Server.List(ctx, hcloud.ServerListOpts{
			ListOpts: hcloud.ListOpts{LabelSelector: selector},
		})
		if err != nil {
			return false, err
		}
		if attempt == 1 || attempt%6 == 0 {
			ready := 0
			for _, server := range servers {
				if server.Status == hcloud.ServerStatusRunning {
					ready++
				}
			}
			infof("server readiness: %d/%d running", ready, expected)
		}
		if len(servers) != expected {
			return false, nil
		}
		for _, server := range servers {
			if server.Status != hcloud.ServerStatusRunning {
				return false, nil
			}
		}
		return true, nil
	})
}

func selectCheapestServerType(ctx context.Context, client *hcloud.Client, locationName string) (string, error) {
	location, _, err := client.Location.GetByName(ctx, locationName)
	if err != nil {
		return "", err
	}
	if location == nil {
		return "", fmt.Errorf("unknown location %q", locationName)
	}

	serverTypes, err := client.ServerType.All(ctx)
	if err != nil {
		return "", err
	}

	bestName := ""
	bestPrice := 0.0
	for _, serverType := range serverTypes {
		if serverType == nil {
			continue
		}
		if serverType.Architecture != hcloud.ArchitectureX86 {
			continue
		}
		if serverType.CPUType != hcloud.CPUTypeShared {
			continue
		}
		if !serverTypeAvailableAtLocation(serverType, location.Name) {
			continue
		}
		price, ok := serverTypeHourlyGross(serverType, location.Name)
		if !ok {
			continue
		}
		if bestName == "" || price < bestPrice || (price == bestPrice && serverType.Name < bestName) {
			bestName = serverType.Name
			bestPrice = price
		}
	}

	if bestName == "" {
		return "", fmt.Errorf("no orderable shared x86 server types found for location %q", location.Name)
	}
	return bestName, nil
}

func serverTypeAvailableAtLocation(serverType *hcloud.ServerType, locationName string) bool {
	for _, loc := range serverType.Locations {
		if loc.Location == nil || loc.Location.Name != locationName {
			continue
		}
		return !loc.IsDeprecated()
	}
	return false
}

func serverTypeHourlyGross(serverType *hcloud.ServerType, locationName string) (float64, bool) {
	for _, pricing := range serverType.Pricings {
		if pricing.Location == nil || pricing.Location.Name != locationName {
			continue
		}
		value, err := strconv.ParseFloat(pricing.Hourly.Gross, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

func waitForSSH(ctx context.Context, keyPath, host string) error {
	attempt := 0
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Minute, Interval: 3 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(attemptCtx, "ssh", sshArgs(keyPath, host, "true")...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if attempt == 1 || attempt%6 == 0 {
				message := strings.TrimSpace(string(output))
				if message == "" {
					message = err.Error()
				}
				infof("still waiting for ssh on %s: %s", host, message)
			}
			return false, nil
		}
		return true, nil
	})
}

func waitForRemoteCommand(ctx context.Context, keyPath, host, remoteCmd string) error {
	attempt := 0
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 5 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(attemptCtx, "ssh", sshArgs(keyPath, host, remoteCmd)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if attempt == 1 || attempt%6 == 0 {
				message := strings.TrimSpace(string(output))
				if message == "" {
					message = err.Error()
				}
				infof("still waiting for remote readiness on %s: %s", host, message)
			}
			return false, nil
		}
		return true, nil
	})
}

func waitForCloudInit(ctx context.Context, keyPath, host string) error {
	attempt := 0
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(attemptCtx, "ssh", sshArgs(keyPath, host, "if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait; else true; fi")...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if attempt == 1 || attempt%6 == 0 {
				message := strings.TrimSpace(string(output))
				if message == "" {
					message = err.Error()
				}
				infof("still waiting for cloud-init on %s: %s", host, message)
			}
			return false, nil
		}
		return true, nil
	})
}

func fetchClientIdentity(ctx context.Context, keyPath, host string) (clientIdentity, error) {
	// Mint a dashboard mTLS client cert offline from the control-plane CA.
	// CN must match the controlplane -dashboard-service-caller-id default ("dashboard").
	remote := fmt.Sprintf(
		"/opt/ebpf-wg-mesh/internal-client-cert -state-dir /var/lib/ebpf-wg-mesh/controlplane -caller-class dashboard -caller-id %s",
		vmDashboardCallerID,
	)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, remote)...)
	output, err := cmd.Output()
	if err != nil {
		return clientIdentity{}, err
	}
	var identity clientIdentity
	if err := json.Unmarshal(output, &identity); err != nil {
		return clientIdentity{}, err
	}
	return identity, nil
}

func runServiceRolloutScenario(ctx context.Context, address string, identity clientIdentity, sshKeyPath string, hosts map[string]hostInfo) error {
	scenarioStarted := time.Now()
	infof("scenario: preparing client identity material")
	caPEM, certPEM, keyPEM, err := identityMaterial(identity)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("append ca pem")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	infof("scenario: dialing controlplane grpc at %s", address)
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	conn, err := grpc.DialContext(dialCtx, address,
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(vmUserAssertionCredentials{secret: vmUserAssertionSecret, userID: vmUserID}),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			ServerName:   "controlplane",
			MinVersion:   tls.VersionTLS13,
		})),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	infof("scenario: grpc connection established after %s", time.Since(scenarioStarted).Round(time.Second))

	client := platformv1.NewPlatformServiceClient(conn)
	userCtx := ctx
	if _, err := waitForAgents(ctx, userCtx, client, 2, sshKeyPath, hosts); err != nil {
		return err
	}

	projectName := "vm-rollout-" + time.Now().UTC().Format("150405")
	infof("scenario: creating project %q", projectName)
	createCtx, cancelCreate := context.WithTimeout(userCtx, 20*time.Second)
	defer cancelCreate()
	project, err := client.CreateProject(createCtx, &platformv1.CreateProjectRequest{
		Name: projectName,
	})
	if err != nil {
		return err
	}
	infof("scenario: project created with id %s", project.GetId())
	environments, err := client.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(environments.GetEnvironments()) != 1 {
		return fmt.Errorf("load production environment: environments=%d err=%w", len(environments.GetEnvironments()), err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()

	volumeName := "data"
	infof("scenario: creating volume %q", volumeName)
	volumeCtx, cancelVolume := context.WithTimeout(userCtx, 20*time.Second)
	defer cancelVolume()
	volume, err := client.CreateVolume(volumeCtx, &platformv1.CreateVolumeRequest{
		EnvironmentId: environmentID,
		Name:          volumeName,
		SizeBytes:     64 * 1024 * 1024,
	})
	if err != nil {
		return err
	}
	if volume.GetId() == "" {
		return fmt.Errorf("volume creation returned an empty id")
	}

	markerV1 := fmt.Sprintf("vm-e2e-v1-%08x", rand.Uint32())
	infof("scenario: creating service with marker %q on volume %q", markerV1, volumeName)
	serviceCtx, cancelService := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelService()
	service, err := client.CreateService(serviceCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: volumeBackedHTTPServiceSpec(markerV1, volumeName),
		},
	})
	if err != nil {
		return err
	}
	if _, err := client.DeployEnvironment(userCtx, &platformv1.DeployEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		return err
	}
	status, err := waitForServiceHealthy(ctx, userCtx, client, service.GetId(), 1, 1)
	if err != nil {
		return err
	}
	allocatedAgentID := status.GetAllocation().GetAgentId()
	allocatedHost, ok := hosts[allocatedAgentID]
	if !ok {
		return fmt.Errorf("no host metadata for allocated service agent %q", allocatedAgentID)
	}
	boundHost := allocatedHost
	allocationID := status.GetAllocation().GetAllocationId()
	endpoint := allocationEndpoint(status.GetAllocation())

	// Host-network curls to workload ULAs are denied by the eBPF veth firewall (only same-identity
	// workload traffic is allowed). Probe the way the agent does: enter the allocation netns.
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, allocationID, endpoint, "/index.html", markerV1); err != nil {
		return fmt.Errorf("verify service response in allocation netns: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, allocatedHost.PublicIPv4, fmt.Sprintf("test -f /var/lib/ebpf-wg-mesh/agent/desired/%s.json && test -f /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html && grep -Fqx %q /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html && ctr --namespace default containers list | awk '{print $1}' | grep -Fx %q >/dev/null", allocationID, volume.GetId(), markerV1, volume.GetId(), managedContainerName(allocationID))); err != nil {
		return fmt.Errorf("verify allocated host state on %s: %w", allocatedHost.Name, err)
	}

	markerV2 := fmt.Sprintf("vm-e2e-v2-%08x", rand.Uint32())
	infof("scenario: updating service rollout to marker %q", markerV2)
	updateCtx, cancelUpdate := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelUpdate()
	updatedService, err := client.UpdateService(updateCtx, &platformv1.UpdateServiceRequest{
		ServiceId: service.GetId(),
		Service: &platformv1.ServiceUpdate{
			Spec: volumeBackedHTTPServiceSpec(markerV2, volumeName),
		},
	})
	if err != nil {
		return err
	}
	if updatedService.GetSpecRevision() < 2 {
		return fmt.Errorf("expected staged revision after update, got spec_revision=%d rollout_generation=%d", updatedService.GetSpecRevision(), updatedService.GetRolloutGeneration())
	}
	redeployCtx, cancelRedeploy := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelRedeploy()
	redeployed, err := client.RedeployService(redeployCtx, &platformv1.RedeployServiceRequest{
		ServiceId: service.GetId(),
	})
	if err != nil {
		return err
	}

	status, err = waitForServiceHealthy(ctx, userCtx, client, service.GetId(), updatedService.GetSpecRevision(), redeployed.GetService().GetRolloutGeneration())
	if err != nil {
		return err
	}
	allocationID = status.GetAllocation().GetAllocationId()
	endpoint = allocationEndpoint(status.GetAllocation())
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, allocationID, endpoint, "/index.html", markerV2); err != nil {
		return fmt.Errorf("verify updated service response in allocation netns: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, allocatedHost.PublicIPv4, fmt.Sprintf("grep -Fqx %q /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html", markerV2, volume.GetId())); err != nil {
		return fmt.Errorf("verify updated volume contents on %s: %w", allocatedHost.Name, err)
	}

	infof("scenario: creating a second-project workload for the mesh isolation check")
	isolationProject, err := client.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "vm-isolation-" + time.Now().UTC().Format("150405")})
	if err != nil {
		return err
	}
	isolationEnvironments, err := client.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: isolationProject.GetId()})
	if err != nil || len(isolationEnvironments.GetEnvironments()) != 1 {
		return fmt.Errorf("load isolation environment: %w", err)
	}
	isolationEnvironmentID := isolationEnvironments.GetEnvironments()[0].GetId()
	isolationMarker := fmt.Sprintf("vm-e2e-isolated-%08x", rand.Uint32())
	isolationService, err := client.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: isolationEnvironmentID,
		Service: &platformv1.ServiceInput{
			Name: "isolated-web",
			Spec: inMemoryHTTPServiceSpec(isolationMarker),
		},
	})
	if err != nil {
		return err
	}
	if _, err := client.DeployEnvironment(userCtx, &platformv1.DeployEnvironmentRequest{EnvironmentId: isolationEnvironmentID}); err != nil {
		return err
	}
	isolationStatus, err := waitForServiceHealthy(ctx, userCtx, client, isolationService.GetId(), isolationService.GetSpecRevision(), 1)
	if err != nil {
		return err
	}
	isolationHost, ok := hosts[isolationStatus.GetAllocation().GetAgentId()]
	if !ok {
		return fmt.Errorf("no host metadata for isolated service agent %q", isolationStatus.GetAllocation().GetAgentId())
	}
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, isolationHost.PublicIPv4, isolationStatus.GetAllocation().GetAllocationId(), allocationEndpoint(isolationStatus.GetAllocation()), "/", isolationMarker); err != nil {
		return fmt.Errorf("verify isolated service is healthy in allocation netns: %w", err)
	}
	if err := assertHTTPResponseFromContainer(ctx, sshKeyPath, allocatedHost.PublicIPv4, managedContainerName(status.GetAllocation().GetAllocationId()), allocationEndpoint(status.GetAllocation()), "/index.html", markerV2); err != nil {
		return fmt.Errorf("same-project mesh success control failed: %w", err)
	}
	if err := assertHTTPDeniedFromContainer(ctx, sshKeyPath, allocatedHost.PublicIPv4, managedContainerName(status.GetAllocation().GetAllocationId()), allocationEndpoint(isolationStatus.GetAllocation()), "/"); err != nil {
		return fmt.Errorf("cross-project mesh isolation failed: %w", err)
	}
	infof("scenario: cross-project workload traffic denied with same-project success control")

	// Stateless-only: node-bound volumes stay pinned and surface Unavailable instead of moving.
	if err := runAgentFailureRollover(ctx, userCtx, client, sshKeyPath, hosts, environmentID); err != nil {
		return err
	}

	infof("scenario: deleting service %s", service.GetId())
	deleteCtx, cancelDelete := context.WithTimeout(userCtx, 20*time.Second)
	defer cancelDelete()
	if _, err := client.DeleteService(deleteCtx, &platformv1.DeleteServiceRequest{
		ServiceId: service.GetId(),
	}); err != nil {
		return err
	}
	if err := waitForServiceDeletion(ctx, userCtx, client, environmentID, service.GetId()); err != nil {
		return err
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, allocatedHost.PublicIPv4, fmt.Sprintf("! test -f /var/lib/ebpf-wg-mesh/agent/desired/%s.json && ! ctr --namespace default containers list | awk '{print $1}' | grep -Fx %q >/dev/null", status.GetAllocation().GetAllocationId(), managedContainerName(status.GetAllocation().GetAllocationId()))); err != nil {
		return fmt.Errorf("verify service teardown on %s: %w", allocatedHost.Name, err)
	}

	infof("scenario: deleting volume %s", volume.GetId())
	deleteVolumeCtx, cancelDeleteVolume := context.WithTimeout(userCtx, 20*time.Second)
	defer cancelDeleteVolume()
	if _, err := client.DeleteVolume(deleteVolumeCtx, &platformv1.DeleteVolumeRequest{
		VolumeId: volume.GetId(),
	}); err != nil {
		return err
	}
	if err := waitForVolumeDeletion(ctx, userCtx, client, environmentID, volume.GetId()); err != nil {
		return err
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, boundHost.PublicIPv4, fmt.Sprintf("! test -e /var/lib/ebpf-wg-mesh/agent/volumes/%s", volume.GetId())); err != nil {
		return fmt.Errorf("verify volume teardown on %s: %w", boundHost.Name, err)
	}

	infof("scenario: completed in %s", time.Since(scenarioStarted).Round(time.Second))
	return nil
}

// runAgentFailureRollover stops the agent hosting a stateless service and waits for
// control-plane expiry failover to reschedule it onto the surviving agent.
func runAgentFailureRollover(ctx, userCtx context.Context, client platformv1.PlatformServiceClient, sshKeyPath string, hosts map[string]hostInfo, environmentID string) error {
	failoverMarker := fmt.Sprintf("vm-e2e-failover-%08x", rand.Uint32())
	infof("scenario: creating stateless failover service with marker %q", failoverMarker)
	createCtx, cancelCreate := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelCreate()
	failoverService, err := client.CreateService(createCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "failover-web",
			Spec: inMemoryHTTPServiceSpec(failoverMarker),
		},
	})
	if err != nil {
		return fmt.Errorf("create failover service: %w", err)
	}
	if _, err := client.DeployEnvironment(userCtx, &platformv1.DeployEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		return fmt.Errorf("deploy failover service: %w", err)
	}
	before, err := waitForServiceHealthy(ctx, userCtx, client, failoverService.GetId(), failoverService.GetSpecRevision(), 1)
	if err != nil {
		return fmt.Errorf("wait for failover service initial health: %w", err)
	}
	failedAgentID := before.GetAllocation().GetAgentId()
	failedHost, ok := hosts[failedAgentID]
	if !ok {
		return fmt.Errorf("no host metadata for failover agent %q", failedAgentID)
	}
	survivingAgentID := ""
	for agentID := range hosts {
		if agentID == "controlplane" || agentID == failedAgentID {
			continue
		}
		survivingAgentID = agentID
		break
	}
	if survivingAgentID == "" {
		return errors.New("need at least two agents for failure rollover")
	}
	survivingHost := hosts[survivingAgentID]

	infof("scenario: stopping agent %s to trigger failure rollover (expect move to %s)", failedAgentID, survivingAgentID)
	if _, err := runRemoteCommand(ctx, sshKeyPath, failedHost.PublicIPv4, "systemctl stop ebpf-wg-mesh-agent"); err != nil {
		return fmt.Errorf("stop agent %s: %w", failedAgentID, err)
	}
	// Always restart so later scenario steps and artifact collection can still talk to the node.
	defer func() {
		infof("scenario: restarting agent %s after rollover check", failedAgentID)
		if _, err := runRemoteCommand(context.Background(), sshKeyPath, failedHost.PublicIPv4, "systemctl start ebpf-wg-mesh-agent"); err != nil {
			infof("scenario: restart agent %s failed: %v", failedAgentID, err)
		}
	}()

	// agentHealthyTTL is 30s; leave headroom for expiry evaluation + pull/start on the peer.
	after, err := waitForServiceOnAgent(ctx, userCtx, client, failoverService.GetId(), survivingAgentID, failoverService.GetSpecRevision(), 1)
	if err != nil {
		return fmt.Errorf("wait for failover onto %s: %w", survivingAgentID, err)
	}
	if after.GetAllocation().GetAgentId() == failedAgentID {
		return fmt.Errorf("service remained on failed agent %s", failedAgentID)
	}
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, survivingHost.PublicIPv4, after.GetAllocation().GetAllocationId(), allocationEndpoint(after.GetAllocation()), "/", failoverMarker); err != nil {
		return fmt.Errorf("verify failover service on surviving agent: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, survivingHost.PublicIPv4, fmt.Sprintf("ctr --namespace default containers list | awk '{print $1}' | grep -Fx %q >/dev/null", managedContainerName(after.GetAllocation().GetAllocationId()))); err != nil {
		return fmt.Errorf("verify failover container on %s: %w", survivingHost.Name, err)
	}
	infof("scenario: agent failure rollover moved service from %s to %s", failedAgentID, after.GetAllocation().GetAgentId())

	deleteCtx, cancelDelete := context.WithTimeout(userCtx, 20*time.Second)
	defer cancelDelete()
	if _, err := client.DeleteService(deleteCtx, &platformv1.DeleteServiceRequest{ServiceId: failoverService.GetId()}); err != nil {
		return fmt.Errorf("delete failover service: %w", err)
	}
	if err := waitForServiceDeletion(ctx, userCtx, client, environmentID, failoverService.GetId()); err != nil {
		return fmt.Errorf("wait for failover service deletion: %w", err)
	}
	return nil
}

type vmUserAssertionCredentials struct {
	secret string
	userID string
}

func (c vmUserAssertionCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    "managed-dashboard",
		Audience:  jwt.ClaimStrings{"controlplane"},
		Subject:   c.userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Second)),
		ID:        fmt.Sprintf("vm-e2e-%d", now.UnixNano()),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(c.secret))
	if err != nil {
		return nil, err
	}
	return map[string]string{"x-platform-user-assertion": token}, nil
}

func (vmUserAssertionCredentials) RequireTransportSecurity() bool { return true }

func waitForAgents(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, expected int, sshKeyPath string, hosts map[string]hostInfo) ([]*platformv1.Agent, error) {
	infof("scenario: waiting for at least %d agents to register", expected)
	waitStarted := time.Now()
	attempt := 0
	var ready []*platformv1.Agent
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		rpcCtx, cancelList := context.WithTimeout(userCtx, 10*time.Second)
		defer cancelList()
		resp, err := client.ListAgents(rpcCtx, &emptypb.Empty{})
		if err != nil {
			if attempt == 1 || attempt%6 == 0 {
				infof("scenario: still waiting for agents after %s (attempt %d): list agents failed: %v", time.Since(waitStarted).Round(time.Second), attempt, err)
			}
			return false, nil
		}
		agents := resp.GetAgents()
		if len(agents) >= expected {
			ready = agents
			infof("scenario: observed %d agents after %s", len(agents), time.Since(waitStarted).Round(time.Second))
			return true, nil
		}
		if attempt == 1 || attempt%6 == 0 {
			agentIDs := make([]string, 0, len(agents))
			for _, agent := range agents {
				agentIDs = append(agentIDs, agent.GetId())
			}
			if len(agentIDs) == 0 {
				infof("scenario: still waiting for agents after %s (attempt %d): none registered yet", time.Since(waitStarted).Round(time.Second), attempt)
			} else {
				infof("scenario: still waiting for agents after %s (attempt %d): have %d/%d agents: %s", time.Since(waitStarted).Round(time.Second), attempt, len(agentIDs), expected, strings.Join(agentIDs, ", "))
			}
			if attempt == 1 || attempt%12 == 0 {
				for key, host := range hosts {
					if key == "controlplane" {
						continue
					}
					dumpRemoteDiagnostics(ctx, sshKeyPath, host)
				}
			}
		}
		return false, nil
	})
	if err != nil {
		for key, host := range hosts {
			if key == "controlplane" {
				continue
			}
			dumpRemoteDiagnostics(ctx, sshKeyPath, host)
		}
	}
	return ready, err
}

func dumpRemoteDiagnostics(ctx context.Context, sshKeyPath string, host hostInfo) {
	cmd := strings.Join([]string{
		"echo '=== systemctl ==='",
		"systemctl status ebpf-wg-mesh-agent ebpf-wg-mesh-controlplane --no-pager || true",
		"echo '=== journal (agent/controlplane, last 80) ==='",
		"journalctl -u ebpf-wg-mesh-agent -u ebpf-wg-mesh-controlplane --no-pager -n 80 || true",
		"echo '=== connectivity ==='",
		"ip -brief addr || true",
		"ss -ltn | head -40 || true",
	}, "; ")
	out, err := runRemoteCommand(ctx, sshKeyPath, host.PublicIPv4, cmd)
	message := strings.TrimSpace(string(out))
	if err != nil && message == "" {
		message = err.Error()
	}
	infof("diagnostics from %s:\n%s", host.Name, message)
}

func waitForServiceHealthy(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, serviceID string, specRevision, rolloutGeneration int64) (*platformv1.ServiceStatus, error) {
	return waitForServiceHealthyOnAgent(ctx, userCtx, client, serviceID, "", specRevision, rolloutGeneration)
}

func waitForServiceOnAgent(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, serviceID, agentID string, specRevision, rolloutGeneration int64) (*platformv1.ServiceStatus, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("agent id is required")
	}
	return waitForServiceHealthyOnAgent(ctx, userCtx, client, serviceID, agentID, specRevision, rolloutGeneration)
}

func waitForServiceHealthyOnAgent(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, serviceID, requiredAgentID string, specRevision, rolloutGeneration int64) (*platformv1.ServiceStatus, error) {
	if requiredAgentID == "" {
		infof("scenario: waiting for service %s rollout spec=%d generation=%d to become healthy", serviceID, specRevision, rolloutGeneration)
	} else {
		infof("scenario: waiting for service %s to become healthy on agent %s (spec=%d generation=%d)", serviceID, requiredAgentID, specRevision, rolloutGeneration)
	}
	waitStarted := time.Now()
	attempt := 0
	var latest *platformv1.ServiceStatus
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		attempt++
		rpcCtx, cancelStatus := context.WithTimeout(userCtx, 10*time.Second)
		defer cancelStatus()
		status, err := client.GetServiceStatus(rpcCtx, &platformv1.GetServiceStatusRequest{
			ServiceId: serviceID,
		})
		if err != nil {
			if attempt == 1 || attempt%6 == 0 {
				infof("scenario: still waiting for service %s after %s (attempt %d): status failed: %v", serviceID, time.Since(waitStarted).Round(time.Second), attempt, err)
			}
			return false, nil
		}
		latest = status
		allocation := status.GetAllocation()
		onRequiredAgent := requiredAgentID == "" || allocation.GetAgentId() == requiredAgentID
		if allocation != nil &&
			onRequiredAgent &&
			allocation.GetHealthy() &&
			allocation.GetAppliedSpecRevision() >= specRevision &&
			allocation.GetAppliedRolloutGeneration() >= rolloutGeneration &&
			allocationEndpoint(allocation) != "" {
			infof("scenario: service %s healthy on agent %s with endpoint %s after %s", serviceID, allocation.GetAgentId(), allocationEndpoint(allocation), time.Since(waitStarted).Round(time.Second))
			return true, nil
		}
		if attempt == 1 || attempt%6 == 0 {
			infof("scenario: still waiting for service %s after %s (attempt %d): agent=%q want=%q phase=%q healthy=%v applied_spec=%d/%d applied_rollout=%d/%d endpoint=%q message=%q", serviceID, time.Since(waitStarted).Round(time.Second), attempt, allocation.GetAgentId(), requiredAgentID, allocation.GetPhase(), allocation.GetHealthy(), allocation.GetAppliedSpecRevision(), specRevision, allocation.GetAppliedRolloutGeneration(), rolloutGeneration, allocationEndpoint(allocation), allocation.GetMessage())
		}
		return false, nil
	})
	return latest, err
}

func waitForServiceDeletion(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, environmentID, serviceID string) error {
	infof("scenario: waiting for service %s deletion to propagate", serviceID)
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		rpcCtx, cancelList := context.WithTimeout(userCtx, 10*time.Second)
		defer cancelList()
		resp, err := client.ListServices(rpcCtx, &platformv1.ListServicesRequest{EnvironmentId: environmentID})
		if err != nil {
			return false, nil
		}
		for _, service := range resp.GetServices() {
			if service.GetId() == serviceID {
				return false, nil
			}
		}
		return true, nil
	})
}

func waitForVolumeDeletion(ctx context.Context, userCtx context.Context, client platformv1.PlatformServiceClient, environmentID, volumeID string) error {
	infof("scenario: waiting for volume %s deletion to propagate", volumeID)
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		rpcCtx, cancelList := context.WithTimeout(userCtx, 10*time.Second)
		defer cancelList()
		resp, err := client.ListVolumes(rpcCtx, &platformv1.ListVolumesRequest{EnvironmentId: environmentID})
		if err != nil {
			return false, nil
		}
		for _, volume := range resp.GetVolumes() {
			if volume.GetId() == volumeID {
				return false, nil
			}
		}
		return true, nil
	})
}

// assertHTTPResponseInAllocationNetNS curls the service from inside its workload netns.
// Host-network access to workload ULAs is dropped by the eBPF mesh firewall on the veth
// (handle_container_egress only allows same-identity peers or reverse-conntrack replies).
func assertHTTPResponseInAllocationNetNS(ctx context.Context, sshKeyPath, host, allocationID, endpointAddr, path, expected string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	url := "http://" + endpointAddr + path
	cmd := fmt.Sprintf(
		`ns=$(cat /var/lib/ebpf-wg-mesh/agent/netns/%s.path); body=$(nsenter --net="$ns" wget -q -T 10 -O - %q); test "$body" = %q`,
		allocationID, url, expected,
	)
	return waitForRemoteCommand(ctx, sshKeyPath, host, cmd)
}

func assertHTTPResponseFromContainer(ctx context.Context, sshKeyPath, host, containerName, endpointAddr, path, expected string) error {
	url := "http://" + endpointAddr + path
	execID := fmt.Sprintf("mesh-allow-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("body=$(ctr --namespace default task exec --exec-id %q %q wget -q -T 10 -O - %q); test \"$body\" = %q", execID, containerName, url, expected)
	return waitForRemoteCommand(ctx, sshKeyPath, host, cmd)
}

func assertHTTPDeniedFromContainer(ctx context.Context, sshKeyPath, host, containerName, endpointAddr, path string) error {
	url := "http://" + endpointAddr + path
	execID := fmt.Sprintf("mesh-deny-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("! ctr --namespace default task exec --exec-id %q %q wget -q -T 5 -O /dev/null %q", execID, containerName, url)
	return waitForRemoteCommand(ctx, sshKeyPath, host, cmd)
}

func allocationEndpoint(allocation *platformv1.AllocationStatus) string {
	if allocation == nil || strings.TrimSpace(allocation.GetAllocationIp()) == "" || len(allocation.GetHealthyPorts()) == 0 {
		return ""
	}
	return net.JoinHostPort(allocation.GetAllocationIp(), strconv.Itoa(int(allocation.GetHealthyPorts()[0])))
}

func volumeBackedHTTPServiceSpec(marker, volumeName string) *platformv1.ServiceSpec {
	return &platformv1.ServiceSpec{
		Runtime: &platformv1.ServiceRuntime{
			Command:         []string{"sh", "-c"},
			Args:            []string{"printf '%s\\n' \"$MARKER\" > /data/index.html && exec httpd -f -p 8080 -h /data"},
			Env:             map[string]string{"MARKER": marker},
			CpuMillis:       250,
			MemoryMebibytes: 256,
			Ports: []*platformv1.ServiceRuntimePort{{
				Port:    8080,
				Primary: true,
			}},
			HealthCheck: &platformv1.HealthCheck{
				Type:           platformv1.HealthCheck_TYPE_HTTP,
				Path:           "/index.html",
				Port:           8080,
				TimeoutSeconds: 2,
			},
			VolumeName: volumeName,
		},
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{
					Image: "docker.io/library/busybox:1.36.1",
				},
			},
		},
	}
}

func inMemoryHTTPServiceSpec(marker string) *platformv1.ServiceSpec {
	return &platformv1.ServiceSpec{
		Runtime: &platformv1.ServiceRuntime{
			Command:         []string{"sh", "-c"},
			Args:            []string{"mkdir -p /tmp/www && printf '%s\\n' \"$MARKER\" > /tmp/www/index.html && exec httpd -f -p 8080 -h /tmp/www"},
			Env:             map[string]string{"MARKER": marker},
			CpuMillis:       250,
			MemoryMebibytes: 256,
			Ports:           []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
			HealthCheck: &platformv1.HealthCheck{
				Type:           platformv1.HealthCheck_TYPE_HTTP,
				Path:           "/",
				Port:           8080,
				TimeoutSeconds: 2,
			},
		},
		Source: &platformv1.ServiceSource{Source: &platformv1.ServiceSource_Image{Image: &platformv1.DirectImageSource{Image: "docker.io/library/busybox:1.36.1"}}},
	}
}

func crossNodeProbeHost(excludeAgentID string, hosts map[string]hostInfo) *hostInfo {
	for agentID, host := range hosts {
		if agentID == "controlplane" || agentID == excludeAgentID {
			continue
		}
		hostCopy := host
		return &hostCopy
	}
	return nil
}

func managedContainerName(allocationID string) string {
	return "platform-" + allocationID
}

func collectArtifacts(ctx context.Context, repoRoot, artifactRoot, keyPath string, hosts map[string]hostInfo) error {
	for name, host := range hosts {
		hostDir := filepath.Join(artifactRoot, "hosts", name)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return err
		}
		commands := map[string]string{
			"journal.txt":   "journalctl -u ebpf-wg-mesh-controlplane -u ebpf-wg-mesh-agent -u ebpf-wg-mesh-cockroach --no-pager || true",
			"ctr.txt":       "ctr --namespace default containers list || true; ctr --namespace default tasks list || true",
			"wg.txt":        "wg show || true",
			"network.txt":   "ip -brief addr || true; ss -ltnup || true",
			"systemd.txt":   "systemctl status ebpf-wg-mesh-controlplane ebpf-wg-mesh-agent ebpf-wg-mesh-cockroach --no-pager || true",
			"processes.txt": "ps aux | grep -E 'controlplane|agent|cockroach|containerd' | grep -v grep || true",
		}
		if host.Role == "controlplane" {
			commands["db.txt"] = "cockroach sql --insecure --host=127.0.0.1:26257 --execute \"SELECT * FROM projects; SELECT * FROM project_memberships; SELECT * FROM agents;\" || true"
		}
		for fileName, remoteCmd := range commands {
			out, err := runRemoteCommand(ctx, keyPath, host.PublicIPv4, remoteCmd)
			if err != nil {
				out = append(out, []byte("\nERROR: "+err.Error()+"\n")...)
			}
			if writeErr := os.WriteFile(filepath.Join(hostDir, fileName), out, 0o644); writeErr != nil {
				return writeErr
			}
		}
	}
	return nil
}

func runRemoteScript(ctx context.Context, keyPath, host, scriptPath string, env map[string]string) error {
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		return err
	}
	cmdLine := ""
	for key, value := range env {
		cmdLine += fmt.Sprintf("%s=%q ", key, value)
	}
	cmdLine += "bash -s"
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, cmdLine)...)
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runRemoteCommand(ctx context.Context, keyPath, host, remoteCmd string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(keyPath, host, remoteCmd)...)
	return cmd.CombinedOutput()
}

func copyFile(ctx context.Context, keyPath, source, host, destination string) error {
	args := append(scpBaseArgs(keyPath), source, fmt.Sprintf("root@%s:%s", host, destination))
	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyFromRemote(ctx context.Context, keyPath, host, source, destination string) error {
	args := append(scpBaseArgs(keyPath), fmt.Sprintf("root@%s:%s", host, source), destination)
	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sshArgs(keyPath, host, remoteCmd string) []string {
	args := append([]string(nil), sshBaseArgs(keyPath)...)
	args = append(args, "root@"+host, remoteCmd)
	return args
}

func sshBaseArgs(keyPath string) []string {
	return []string{
		"-i", keyPath,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
}

func scpBaseArgs(keyPath string) []string {
	return append([]string(nil), sshBaseArgs(keyPath)...)
}

func identityMaterial(identity clientIdentity) ([]byte, []byte, []byte, error) {
	ca, err := decodeB64(identity.CAPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := decodeB64(identity.CertPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := decodeB64(identity.KeyPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	return ca, cert, key, nil
}

func decodeB64(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}

func trimCIDR(value string) string {
	return strings.TrimSpace(strings.SplitN(value, "/", 2)[0])
}

func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func failf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

func infof(format string, args ...any) {
	fmt.Printf("[testvm] "+format+"\n", args...)
}
