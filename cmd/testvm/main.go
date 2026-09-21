package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/localteststack"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	vmUserID = "vm-user"
	// Default controlplane dashboard ServiceCallerID; must match minted client cert CN.
	vmDashboardCallerID = "dashboard"

	primaryControlPlaneService = "ebpf-wg-mesh-controlplane"
	replicaControlPlaneService = "ebpf-wg-mesh-controlplane-replica"
	primaryControlPlanePort    = "9443"
	replicaControlPlanePort    = "9444"
	testVMWireGuardPort        = "51820"

	// prepareHostCommand makes a disposable test host deterministic. Scheduled
	// package maintenance (apt-daily) and needrestart can restart the mesh
	// services minutes after boot, which would look like a product fault
	// mid-campaign.
	prepareHostCommand = "mkdir -p /opt/ebpf-wg-mesh; " +
		"systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null 2>&1 || true; " +
		"systemctl mask apt-daily.service apt-daily-upgrade.service unattended-upgrades.service >/dev/null 2>&1 || true; " +
		"mkdir -p /etc/needrestart/conf.d && printf '%s\\n' '$nrconf{restart} = \"l\";' > /etc/needrestart/conf.d/99-ebpf-wg-mesh-testvm.conf"
)

var vmAgentBootstrapTokens = map[string]string{
	"agent-a": "vm-bootstrap-token-agent-a",
	"agent-b": "vm-bootstrap-token-agent-b",
}

// vmUserAssertionSecret is fetched from the control plane's shared signing
// keys after boot; the development control plane generates it.
var vmUserAssertionSecret string

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

func validateAgentBindings(hosts map[string]hostInfo, tokens map[string]string) error {
	for key := range hosts {
		if key == "controlplane" {
			continue
		}
		if _, ok := tokens[key]; !ok {
			return fmt.Errorf("no bootstrap token configured for agent key %q", key)
		}
	}
	for agentID := range tokens {
		if _, ok := hosts[agentID]; !ok {
			return fmt.Errorf("bootstrap token configured for unknown agent %q", agentID)
		}
	}
	return nil
}

func main() {
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
	provider := flag.String("provider", "hetzner", "VM provider: hetzner, ovh, or local")
	timeout := flag.Duration("timeout", 45*time.Minute, "maximum run duration, excluding bounded cleanup")
	ovhOpts := registerOVHFlags()
	stressOpts := registerStressFlags()
	localOpts := registerLocalFlags()
	stressPlanOnly := flag.Bool("stress-plan-only", false, "print seeded stress schedule without credentials or cloud resources")
	flag.Parse()
	if *provider != "hetzner" && *provider != "ovh" && *provider != "local" {
		failf("unknown provider %q", *provider)
	}
	if *scenario != "service-rollout" && *scenario != "stress" {
		failf("unknown scenario %q", *scenario)
	}
	if err := stressOpts.validate(); err != nil {
		failf("stress options: %v", err)
	}

	if *stressPlanOnly {
		agents := ovhOpts.agents
		if *provider == "local" {
			agents = localOpts.agents
		} else if *provider == "hetzner" {
			agents = 2
		}
		maxAgents := 32
		if *provider == "local" {
			maxAgents = 16
		}
		if agents < 2 || agents > maxAgents {
			failf("agents must be between 2 and %d", maxAgents)
		}
		agentNames := stressPlanAgentNames(*provider, agents)
		if err := json.NewEncoder(os.Stdout).Encode(stressSchedule(*stressOpts, agentNames)); err != nil {
			failf("stress plan: %v", err)
		}
		return
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	startedAt := time.Now().UTC()

	repoRoot, err := os.Getwd()
	if err != nil {
		failf("getwd: %v", err)
	}
	if n, err := localteststack.LoadDotEnvFile(filepath.Join(repoRoot, ".env")); err != nil {
		failf("load .env: %v", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[testvm] loaded %d variable(s) from .env\n", n)
	}

	var ovhClient ovhAPI
	var plan ovhPlan
	var token string
	var client *hcloud.Client
	if *provider == "local" {
		if localOpts.action == "destroy" {
			if err := destroyLocal(ctx, localOpts.manifest, localOpts.keepDisks); err != nil {
				failf("local cleanup: %v", err)
			}
			return
		}
		vmAgentBootstrapTokens = make(map[string]string)
		for i := 0; i < localOpts.agents; i++ {
			name := fmt.Sprintf("agent-%02d", i+1)
			vmAgentBootstrapTokens[name] = fmt.Sprintf("vm-bootstrap-%016x-%016x", rand.Uint64(), rand.Uint64())
		}
	} else if *provider == "ovh" {
		ovhEnvPath := filepath.Join(repoRoot, ".env.ovh")
		if info, statErr := os.Stat(ovhEnvPath); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			failf("%s must not be readable by group or others; run chmod 600 %s", ovhEnvPath, ovhEnvPath)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			failf("inspect .env.ovh: %v", statErr)
		}
		if _, err := localteststack.LoadDotEnvFile(ovhEnvPath); err != nil {
			failf("load .env.ovh: %v", err)
		}
		ovhClient, plan, err = prepareOVH(ctx, ovhOpts, *runID, *timeout)
		if err != nil {
			failf("OVH preflight: %v", err)
		}
		switch ovhOpts.action {
		case "catalog":
			return
		case "plan":
			if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
				failf("write plan: %v", err)
			}
			return
		case "destroy":
			if err := destroyOVH(ctx, ovhClient, ovhOpts.manifest); err != nil {
				failf("OVH cleanup: %v", err)
			}
			return
		}
		vmAgentBootstrapTokens = make(map[string]string)
		for i := 0; i < ovhOpts.agents; i++ {
			name := fmt.Sprintf("agent-%02d", i+1)
			vmAgentBootstrapTokens[name] = fmt.Sprintf("vm-bootstrap-%016x-%016x", rand.Uint64(), rand.Uint64())
		}
	} else {
		token = strings.TrimSpace(os.Getenv("HCLOUD_TOKEN"))
		if token == "" {
			failf("missing HCLOUD_TOKEN (export it or set it in repo-root .env)")
		}
		client = hcloud.NewClient(hcloud.WithToken(token))
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
		"controlplane": filepath.Join(binDir, "controlplane"),
		"agent":        filepath.Join(binDir, "agent"),
	}
	infof("building Linux binaries")
	if err := buildBinaries(ctx, repoRoot, binaries); err != nil {
		failf("build binaries: %v", err)
	}

	var hosts map[string]hostInfo
	var destroy func() error
	destroyed := false
	defer func() {
		if destroy != nil && !destroyed {
			infof("destroying VM environment")
			if err := destroy(); err != nil {
				if p := recover(); p != nil {
					infof("VM cleanup failed during existing failure: %v", err)
					panic(p)
				}
				failf("VM cleanup failed: %v", err)
			}
		}
	}()
	defer func() {
		if destroyed {
			return
		}
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := collectArtifacts(diagnosticCtx, repoRoot, artifactRoot, sshKeyPath, hosts); err != nil {
			infof("collect diagnostics: %v", err)
		}
	}()
	if *provider == "local" {
		localPlan, err := prepareLocal(ctx, *localOpts, *runID, *timeout)
		if err != nil {
			failf("local prepare: %v", err)
		}
		if err := writeJSON(filepath.Join(artifactRoot, "local-plan.json"), localPlan); err != nil {
			failf("write local plan: %v", err)
		}
		hosts, destroy, err = provisionLocal(ctx, *localOpts, localPlan, artifactRoot, sshKeyPath)
		if err != nil {
			failf("provision local: %v", err)
		}
	} else if *provider == "ovh" {
		if err := writeJSON(filepath.Join(artifactRoot, "ovh-plan.json"), plan); err != nil {
			failf("write OVH plan: %v", err)
		}
		hosts, destroy, err = provisionOVH(ctx, ovhClient, plan, repoRoot, artifactRoot, sshKeyPath)
		if err != nil {
			failf("provision OVH: %v", err)
		}
	} else {
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

		destroy = func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			return runCommand(cleanupCtx, repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "destroy", "-auto-approve", "-input=false", "-var-file="+varsPath)
		}
		infof("applying vm environment in %s", *location)
		if err := runCommand(ctx, repoRoot, tofuEnv, *tofuBin, "-chdir="+*tofuDir, "apply", "-auto-approve", "-input=false", "-var-file="+varsPath); err != nil {
			failf("tofu apply: %v", err)
		}

		infof("reading provisioned host outputs")
		hosts, err = readHostsOutput(ctx, repoRoot, tofuEnv, *tofuBin, *tofuDir)
		if err != nil {
			failf("read tofu outputs: %v", err)
		}
		infof("waiting for Hetzner to report %d running servers", len(hosts))
		if err := waitForServers(ctx, client, *runID, len(hosts)); err != nil {
			failf("wait for Hetzner servers: %v", err)
		}

	}
	if err := validateAgentBindings(hosts, vmAgentBootstrapTokens); err != nil {
		failf("validate agent bindings: %v", err)
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
		if _, err := runRemoteCommand(ctx, sshKeyPath, host.PublicIPv4, prepareHostCommand); err != nil {
			failf("prepare host %s: %v", host.Name, err)
		}
	}

	controlplane := hosts["controlplane"]
	infof("copying controlplane binaries to %s", controlplane.Name)
	if err := copyFile(ctx, sshKeyPath, binaries["controlplane"], controlplane.PublicIPv4, "/opt/ebpf-wg-mesh/controlplane"); err != nil {
		failf("copy controlplane binary: %v", err)
	}
	infof("installing ingress admin probe on %s", controlplane.Name)
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-ingress-probe.sh"), nil); err != nil {
		failf("install ingress probe: %v", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet ebpf-wg-mesh-ingress-probe && ss -ltn '( sport = :2019 )' | grep -q LISTEN"); err != nil {
		failf("wait for ingress probe readiness: %v", err)
	}

	infof("installing primary controlplane replica on %s", controlplane.Name)
	bootstrapBindings := make([]string, 0, len(vmAgentBootstrapTokens))
	for agentID, token := range vmAgentBootstrapTokens {
		bootstrapBindings = append(bootstrapBindings, agentID+"="+token)
	}
	sort.Strings(bootstrapBindings)
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-controlplane.sh"), map[string]string{
		"PUBLIC_ADDR":            "platform.local",
		"AGENT_BOOTSTRAP_TOKENS": strings.Join(bootstrapBindings, ","),
		"SERVICE_NAME":           primaryControlPlaneService,
		"INTERNAL_LISTEN":        "0.0.0.0:" + primaryControlPlanePort,
		"REPLICA_ADDRESSES":      controlplane.PublicIPv4 + ":" + primaryControlPlanePort + "," + controlplane.PublicIPv4 + ":" + replicaControlPlanePort,
		"ADVERTISE_ADDR":         controlplane.PublicIPv4 + ":" + primaryControlPlanePort,
	}); err != nil {
		failf("install primary controlplane replica: %v", err)
	}
	infof("waiting for primary controlplane readiness on %s", controlplane.Name)
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet "+primaryControlPlaneService+" && test -f /var/lib/ebpf-wg-mesh/controlplane/pki/server.crt && ss -ltn '( sport = :"+primaryControlPlanePort+" )' | grep -q LISTEN"); err != nil {
		failf("wait for primary controlplane readiness: %v", err)
	}
	primaryLease, err := waitForSingletonLease(ctx, sshKeyPath, controlplane.PublicIPv4, "")
	if err != nil {
		failf("wait for primary singleton lease: %v", err)
	}
	infof("primary controlplane acquired singleton lease holder=%s token=%d", primaryLease.Holder, primaryLease.Token)

	infof("installing second controlplane replica on %s", controlplane.Name)
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-controlplane.sh"), map[string]string{
		"PUBLIC_ADDR":            "platform.local",
		"AGENT_BOOTSTRAP_TOKENS": strings.Join(bootstrapBindings, ","),
		"SERVICE_NAME":           replicaControlPlaneService,
		"INTERNAL_LISTEN":        "0.0.0.0:" + replicaControlPlanePort,
		"REPLICA_ADDRESSES":      controlplane.PublicIPv4 + ":" + primaryControlPlanePort + "," + controlplane.PublicIPv4 + ":" + replicaControlPlanePort,
		"ADVERTISE_ADDR":         controlplane.PublicIPv4 + ":" + replicaControlPlanePort,
	}); err != nil {
		failf("install second controlplane replica: %v", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet "+replicaControlPlaneService+" && ss -ltn '( sport = :"+replicaControlPlanePort+" )' | grep -q LISTEN"); err != nil {
		failf("wait for second controlplane readiness: %v", err)
	}
	leaseWithBothReplicas, err := readSingletonLease(ctx, sshKeyPath, controlplane.PublicIPv4)
	if err != nil {
		failf("read singleton lease with both replicas running: %v", err)
	}
	if leaseWithBothReplicas != primaryLease {
		failf("second replica displaced a healthy singleton owner: before=%+v after=%+v", primaryLease, leaseWithBothReplicas)
	}
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/assert-local-storage-rejected.sh"), map[string]string{
		"AGENT_BOOTSTRAP_TOKENS": strings.Join(bootstrapBindings, ","),
	}); err != nil {
		failf("verify replica-local storage rejection: %v", err)
	}
	infof("replica-local controlplane state was rejected as expected")

	caPath := filepath.Join(artifactRoot, "controlplane-ca.crt")
	infof("fetching controlplane ca certificate")
	caPEM, err := exportSigningMaterial(ctx, sshKeyPath, controlplane.PublicIPv4, "internal-ca")
	if err != nil {
		failf("fetch controlplane ca: %v", err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		failf("write controlplane ca: %v", err)
	}
	infof("fetching user assertion secret")
	assertionSecret, err := exportSigningMaterial(ctx, sshKeyPath, controlplane.PublicIPv4, "user-assertion")
	if err != nil {
		failf("fetch user assertion secret: %v", err)
	}
	vmUserAssertionSecret = strings.TrimSpace(string(assertionSecret))
	if vmUserAssertionSecret == "" {
		failf("user assertion secret export is empty")
	}

	for key, host := range hosts {
		if key == "controlplane" {
			continue
		}
		bootstrapToken := vmAgentBootstrapTokens[key]
		infof("copying agent binary to %s", host.Name)
		if err := copyFile(ctx, sshKeyPath, binaries["agent"], host.PublicIPv4, "/opt/ebpf-wg-mesh/agent"); err != nil {
			failf("copy agent binary to %s: %v", host.Name, err)
		}
		if err := copyFile(ctx, sshKeyPath, caPath, host.PublicIPv4, "/opt/ebpf-wg-mesh/controlplane-ca.crt"); err != nil {
			failf("copy ca to %s: %v", host.Name, err)
		}
		infof("installing agent on %s (node-id=%s)", host.Name, key)
		if err := runRemoteScript(ctx, sshKeyPath, host.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-agent.sh"), map[string]string{
			"NODE_ID":                 key,
			"NODE_NAME":               host.Name,
			"ADVERTISE_ADDR":          hostAdvertiseAddress(host),
			"MESH_ADVERTISE_ENDPOINT": hostWireGuardEndpoint(host, *provider),
			"CONTROLPLANE_ADDRESSES":  controlplane.PublicIPv4 + ":" + primaryControlPlanePort + "," + controlplane.PublicIPv4 + ":" + replicaControlPlanePort,
			"BOOTSTRAP_TOKEN":         bootstrapToken,
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
	if *scenario == "stress" {
		if err := runStressScenario(ctx, *stressOpts, artifactRoot, identity, sshKeyPath, hosts, repoRoot); err != nil {
			failf("stress scenario: %v", err)
		}
	} else {
		infof("running cross-replica scenario (owner-local delivery, durable reads and blocking watch on the second replica)")
		fixture, err := runCrossReplicaNotificationScenario(
			ctx,
			controlplane.PublicIPv4+":"+primaryControlPlanePort,
			controlplane.PublicIPv4+":"+replicaControlPlanePort,
			identity,
			sshKeyPath,
			controlplane,
			hosts,
		)
		if err != nil {
			infof("cross-replica scenario failed; collecting diagnostics into %s", artifactRoot)
			_ = collectArtifacts(ctx, repoRoot, artifactRoot, sshKeyPath, hosts)
			failf("run cross-replica notification scenario: %v", err)
		}
		ingressRequestsBeforeTakeover, err := ingressRequestCount(ctx, sshKeyPath, controlplane.PublicIPv4)
		if err != nil {
			failf("read ingress request count before takeover: %v", err)
		}

		infof("stopping primary controlplane to force singleton takeover")
		if _, err := runRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl stop "+primaryControlPlaneService); err != nil {
			failf("stop primary controlplane: %v", err)
		}
		if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet "+replicaControlPlaneService+" && ! systemctl is-active --quiet "+primaryControlPlaneService); err != nil {
			failf("wait for primary controlplane shutdown: %v", err)
		}
		replicaLease, err := waitForSingletonLease(ctx, sshKeyPath, controlplane.PublicIPv4, primaryLease.Holder)
		if err != nil {
			failf("wait for second replica singleton takeover: %v", err)
		}
		if replicaLease.Token <= primaryLease.Token {
			failf("singleton fencing token did not advance on takeover: before=%d after=%d", primaryLease.Token, replicaLease.Token)
		}
		if err := waitForIngressTakeover(ctx, sshKeyPath, controlplane.PublicIPv4, ingressRequestsBeforeTakeover, fixture.Hostname); err != nil {
			failf("wait for ingress convergence after takeover: %v", err)
		}
		infof("second controlplane took singleton lease holder=%s token=%d and republished ingress", replicaLease.Holder, replicaLease.Token)

		if err := cleanupCrossReplicaFixture(ctx, controlplane.PublicIPv4+":"+replicaControlPlanePort, identity, fixture); err != nil {
			failf("clean up cross-replica fixture: %v", err)
		}
		infof("running service rollout scenario against %s", controlplane.Name)
		if err := runServiceRolloutScenario(ctx, controlplane.PublicIPv4+":"+replicaControlPlanePort, identity, sshKeyPath, hosts); err != nil {
			infof("scenario failed; collecting diagnostics into %s", artifactRoot)
			_ = collectArtifacts(ctx, repoRoot, artifactRoot, sshKeyPath, hosts)
			failf("run service rollout scenario: %v", err)
		}

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
	if err := destroy(); err != nil {
		failf("VM cleanup: %v", err)
	}
	destroyed = true
	infof("vm test run completed successfully")
}

func stressPlanAgentNames(provider string, agents int) []string {
	if provider == "hetzner" {
		return []string{"agent-a", "agent-b"}
	}
	names := make([]string, agents)
	for i := range names {
		names[i] = fmt.Sprintf("agent-%02d", i+1)
	}
	return names
}
func runServiceRolloutScenario(ctx context.Context, address string, identity clientIdentity, sshKeyPath string, hosts map[string]hostInfo) error {
	scenarioStarted := time.Now()
	infof("scenario: dialing controlplane grpc at %s", address)
	conn, err := dialPlatform(ctx, address, identity)
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
	if err != nil {
		return fmt.Errorf("load production environment: %w", err)
	}
	if len(environments.GetEnvironments()) != 1 {
		return fmt.Errorf("load production environment: got %d environments, want 1", len(environments.GetEnvironments()))
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
	if _, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
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

	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, allocationID, endpoint, "/index.html", markerV1); err != nil {
		return fmt.Errorf("verify service response in allocation netns: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, allocatedHost.PublicIPv4, fmt.Sprintf("test -f /var/lib/ebpf-wg-mesh/agent/desired/%s.json && test -f /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html && grep -Fqx %q /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html && ctr --namespace default containers list | awk '{print $1}' | grep -Fx %q >/dev/null", allocationID, volume.GetId(), markerV1, volume.GetId(), managedContainerName(allocationID))); err != nil {
		return fmt.Errorf("verify allocated host state on %s: %w", allocatedHost.Name, err)
	}

	markerV2 := fmt.Sprintf("vm-e2e-v2-%08x", rand.Uint32())
	infof("scenario: staging volume-backed service rollout to marker %q", markerV2)
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
	releaseCtx, cancelRelease := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelRelease()
	_, err = client.ReleaseEnvironment(releaseCtx, &platformv1.ReleaseEnvironmentRequest{
		EnvironmentId: environmentID,
	})
	if err == nil {
		return errors.New("expected FailedPrecondition deploying a volume-backed service revision with existing allocations")
	}
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		return fmt.Errorf("deploy volume-backed service revision: got %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "volume-backed services cannot overlap rollout generations until volume handoff is supported") {
		return fmt.Errorf("deploy volume-backed service revision: got %v, want volume-handoff rejection", err)
	}
	infof("scenario: overlapping volume-backed rollout rejected as FailedPrecondition")
	if _, err := client.DiscardServiceChanges(userCtx, &platformv1.DiscardServiceChangesRequest{
		ServiceId:  service.GetId(),
		DiscardAll: true,
	}); err != nil {
		return fmt.Errorf("discard rejected volume-backed service changes: %w", err)
	}

	status, err = waitForServiceHealthy(ctx, userCtx, client, service.GetId(), 1, 1)
	if err != nil {
		return err
	}
	allocationID = status.GetAllocation().GetAllocationId()
	endpoint = allocationEndpoint(status.GetAllocation())
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, allocationID, endpoint, "/index.html", markerV1); err != nil {
		return fmt.Errorf("verify original service response after rejected environment release: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, allocatedHost.PublicIPv4, fmt.Sprintf("grep -Fqx %q /var/lib/ebpf-wg-mesh/agent/volumes/%s/index.html", markerV1, volume.GetId())); err != nil {
		return fmt.Errorf("verify original volume contents on %s: %w", allocatedHost.Name, err)
	}

	infof("scenario: creating a second-project workload for the mesh isolation check")
	isolationProject, err := client.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "vm-isolation-" + time.Now().UTC().Format("150405")})
	if err != nil {
		return err
	}
	isolationEnvironments, err := client.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: isolationProject.GetId()})
	if err != nil {
		return fmt.Errorf("load isolation environment: %w", err)
	}
	if len(isolationEnvironments.GetEnvironments()) != 1 {
		return fmt.Errorf("load isolation environment: got %d environments, want 1", len(isolationEnvironments.GetEnvironments()))
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
	if _, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: isolationEnvironmentID}); err != nil {
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
	if err := assertHTTPResponseFromContainer(ctx, sshKeyPath, allocatedHost.PublicIPv4, managedContainerName(status.GetAllocation().GetAllocationId()), allocationEndpoint(status.GetAllocation()), "/index.html", markerV1); err != nil {
		return fmt.Errorf("same-project mesh success control failed: %w", err)
	}
	if err := assertHTTPDeniedFromContainer(ctx, sshKeyPath, allocatedHost.PublicIPv4, managedContainerName(status.GetAllocation().GetAllocationId()), allocationEndpoint(isolationStatus.GetAllocation()), "/"); err != nil {
		return fmt.Errorf("cross-project mesh isolation failed: %w", err)
	}
	infof("scenario: cross-project workload traffic denied with same-project success control")

	if err := runWorkloadIsolationChecks(ctx, userCtx, client, sshKeyPath, hosts, environmentID, allocatedHost, managedContainerName(status.GetAllocation().GetAllocationId())); err != nil {
		return err
	}

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

func runAgentFailureRollover(ctx, userCtx context.Context, client platformv1.PlatformServiceClient, sshKeyPath string, hosts map[string]hostInfo, environmentID string) error {
	failoverMarker := fmt.Sprintf("vm-e2e-failover-%08x", rand.Uint32())
	infof("scenario: creating stateless failover service with marker %q", failoverMarker)
	createCtx, cancelCreate := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelCreate()
	failoverService, err := client.CreateService(createCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "failover-web",
			Spec: dualStackHTTPServiceSpec(failoverMarker),
		},
	})
	if err != nil {
		return fmt.Errorf("create failover service: %w", err)
	}
	if _, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
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
	beforeEndpoints := allocationEndpoints(before.GetAllocation())
	if len(beforeEndpoints) != 2 {
		return fmt.Errorf("failover service did not become healthy on both families before node loss: %v", beforeEndpoints)
	}
	for _, endpoint := range beforeEndpoints {
		if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, failedHost.PublicIPv4, before.GetAllocation().GetAllocationId(), endpoint, "/", failoverMarker); err != nil {
			return fmt.Errorf("verify pre-failover service endpoint %s: %w", endpoint, err)
		}
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
	defer func() {
		infof("scenario: restarting agent %s after rollover check", failedAgentID)
		if _, err := runRemoteCommand(context.Background(), sshKeyPath, failedHost.PublicIPv4, "systemctl start ebpf-wg-mesh-agent"); err != nil {
			infof("scenario: restart agent %s failed: %v", failedAgentID, err)
		}
	}()

	after, err := waitForServiceOnAgent(ctx, userCtx, client, failoverService.GetId(), survivingAgentID, failoverService.GetSpecRevision(), 1)
	if err != nil {
		return fmt.Errorf("wait for failover onto %s: %w", survivingAgentID, err)
	}
	if after.GetAllocation().GetAgentId() == failedAgentID {
		return fmt.Errorf("service remained on failed agent %s", failedAgentID)
	}
	afterEndpoints := allocationEndpoints(after.GetAllocation())
	if len(afterEndpoints) != 2 {
		return fmt.Errorf("failover service did not recover both families on surviving agent: %v", afterEndpoints)
	}
	for _, endpoint := range afterEndpoints {
		if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, survivingHost.PublicIPv4, after.GetAllocation().GetAllocationId(), endpoint, "/", failoverMarker); err != nil {
			return fmt.Errorf("verify post-failover service endpoint %s: %w", endpoint, err)
		}
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

func hostAdvertiseAddress(host hostInfo) string {
	if host.PublicIPv6 != "" {
		return trimCIDR(host.PublicIPv6)
	}
	return host.PublicIPv4
}

func hostWireGuardEndpoint(host hostInfo, provider string) string {
	address := hostAdvertiseAddress(host)
	if provider == "ovh" || provider == "local" {
		address = host.PublicIPv4
	}
	return net.JoinHostPort(address, testVMWireGuardPort)
}
