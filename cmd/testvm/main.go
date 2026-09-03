package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/localteststack"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	vmUserID              = "vm-user"
	vmUserAssertionSecret = "vm-user-assertion-secret-at-least-32-bytes"
	// Default controlplane dashboard ServiceCallerID; must match minted client cert CN.
	vmDashboardCallerID = "dashboard"

	primaryControlPlaneService = "ebpf-wg-mesh-controlplane"
	replicaControlPlaneService = "ebpf-wg-mesh-controlplane-replica"
	primaryControlPlanePort    = "9443"
	replicaControlPlanePort    = "9444"
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
	// Stable order for systemd unit / logs.
	sort.Strings(bootstrapBindings)
	if err := runRemoteScript(ctx, sshKeyPath, controlplane.PublicIPv4, filepath.Join(repoRoot, "infra/test-vm/remote/install-controlplane.sh"), map[string]string{
		"PUBLIC_ADDR":            "platform.local",
		"AGENT_BOOTSTRAP_TOKENS": strings.Join(bootstrapBindings, ","),
		"USER_ASSERTION_SECRET":  vmUserAssertionSecret,
		"SERVICE_NAME":           primaryControlPlaneService,
		"INTERNAL_LISTEN":        "0.0.0.0:" + primaryControlPlanePort,
	}); err != nil {
		failf("install primary controlplane replica: %v", err)
	}
	infof("waiting for primary controlplane readiness on %s", controlplane.Name)
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, "systemctl is-active --quiet "+primaryControlPlaneService+" && test -f /var/lib/ebpf-wg-mesh/controlplane/pki/ca.crt && ss -ltn '( sport = :"+primaryControlPlanePort+" )' | grep -q LISTEN"); err != nil {
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
		"USER_ASSERTION_SECRET":  vmUserAssertionSecret,
		"SERVICE_NAME":           replicaControlPlaneService,
		"INTERNAL_LISTEN":        "0.0.0.0:" + replicaControlPlanePort,
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
		"USER_ASSERTION_SECRET":  vmUserAssertionSecret,
	}); err != nil {
		failf("verify replica-local storage rejection: %v", err)
	}
	infof("replica-local controlplane state was rejected as expected")

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
			"CONTROLPLANE_ADDRESS": controlplane.PublicIPv4 + ":" + replicaControlPlanePort,
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
	infof("running cross-replica notification scenario (writes on primary, reads and agent streams on replica)")
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
	redeployCtx, cancelRedeploy := context.WithTimeout(userCtx, 30*time.Second)
	defer cancelRedeploy()
	redeployedEnvironment, err := client.DeployEnvironment(redeployCtx, &platformv1.DeployEnvironmentRequest{EnvironmentId: environmentID})
	if err != nil {
		return err
	}
	if len(redeployedEnvironment.GetServices()) != 1 {
		return fmt.Errorf("expected one redeployed service, got %d", len(redeployedEnvironment.GetServices()))
	}
	redeployed := redeployedEnvironment.GetServices()[0]
	status, err = waitForServiceHealthy(ctx, userCtx, client, service.GetId(), updatedService.GetSpecRevision(), redeployed.GetService().GetRolloutGeneration())
	if err != nil {
		return err
	}
	deploymentHistory, err := client.ListServiceDeployments(redeployCtx, &platformv1.ListServiceDeploymentsRequest{ServiceId: service.GetId(), Limit: 1})
	if err != nil {
		return err
	}
	if len(deploymentHistory.GetDeployments()) != 1 {
		return fmt.Errorf("expected one deployment to restart, got %d", len(deploymentHistory.GetDeployments()))
	}
	restartRequest := &platformv1.ApplyDeploymentActionRequest{
		ServiceId:      service.GetId(),
		DeploymentId:   deploymentHistory.GetDeployments()[0].GetId(),
		Action:         platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART,
		IdempotencyKey: "vm-restart-" + deploymentHistory.GetDeployments()[0].GetId(),
		AllocationId:   status.GetAllocation().GetAllocationId(),
	}
	if _, err := client.ApplyDeploymentAction(redeployCtx, restartRequest); err != nil {
		return err
	}
	if _, err := client.ApplyDeploymentAction(redeployCtx, restartRequest); err != nil {
		return err
	}
	_, err = client.RedeployService(redeployCtx, &platformv1.RedeployServiceRequest{
		ServiceId: service.GetId(),
	})
	if err == nil {
		return errors.New("expected FailedPrecondition redeploying a volume-backed service with existing allocations")
	}
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		return fmt.Errorf("redeploy volume-backed service: got %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "volume-backed services cannot overlap rollout generations until volume handoff is supported") {
		return fmt.Errorf("redeploy volume-backed service: got %v, want volume-handoff rejection", err)
	}
	infof("scenario: overlapping volume-backed rollout rejected as FailedPrecondition")

	status, err = waitForServiceHealthy(ctx, userCtx, client, service.GetId(), 1, 1)
	if err != nil {
		return err
	}
	allocationID = status.GetAllocation().GetAllocationId()
	endpoint = allocationEndpoint(status.GetAllocation())
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, allocationID, endpoint, "/index.html", markerV1); err != nil {
		return fmt.Errorf("verify original service response after rejected redeploy: %w", err)
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
			Spec: dualStackHTTPServiceSpec(failoverMarker),
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
