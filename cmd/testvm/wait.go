package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/testutil"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"google.golang.org/protobuf/types/known/emptypb"
)

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
		"systemctl status ebpf-wg-mesh-agent ebpf-wg-mesh-controlplane ebpf-wg-mesh-controlplane-replica ebpf-wg-mesh-ingress-probe --no-pager || true",
		"echo '=== journal (agent/controlplane, last 80) ==='",
		"journalctl -u ebpf-wg-mesh-agent -u ebpf-wg-mesh-controlplane -u ebpf-wg-mesh-controlplane-replica -u ebpf-wg-mesh-ingress-probe --no-pager -n 80 || true",
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
		allocation := matchingHealthyAllocation(status, requiredAgentID, specRevision, rolloutGeneration)
		if allocation != nil {
			latest.Allocation = allocation
			infof("scenario: service %s healthy on agent %s with endpoint %s after %s", serviceID, allocation.GetAgentId(), allocationEndpoint(allocation), time.Since(waitStarted).Round(time.Second))
			return true, nil
		}
		if attempt == 1 || attempt%6 == 0 {
			infof("scenario: still waiting for service %s after %s (attempt %d): want agent=%q spec>=%d rollout>=%d allocations=[%s]", serviceID, time.Since(waitStarted).Round(time.Second), attempt, requiredAgentID, specRevision, rolloutGeneration, formatServiceAllocations(status))
		}
		return false, nil
	})
	return latest, err
}

func matchingHealthyAllocation(status *platformv1.ServiceStatus, requiredAgentID string, specRevision, rolloutGeneration int64) *platformv1.AllocationStatus {
	for _, allocation := range status.GetAllocations() {
		if requiredAgentID != "" && allocation.GetAgentId() != requiredAgentID {
			continue
		}
		if allocation.GetHealthy() &&
			allocation.GetRolloutState() == "serving" &&
			allocation.GetAppliedSpecRevision() >= specRevision &&
			allocation.GetAppliedRolloutGeneration() >= rolloutGeneration &&
			allocationEndpoint(allocation) != "" {
			return allocation
		}
	}
	return nil
}

func formatServiceAllocations(status *platformv1.ServiceStatus) string {
	allocs := status.GetAllocations()
	if len(allocs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(allocs))
	for _, allocation := range allocs {
		parts = append(parts, fmt.Sprintf("id=%s agent=%s phase=%s rollout_state=%s healthy=%v applied_spec=%d applied_rollout=%d endpoint=%q message=%q",
			allocation.GetAllocationId(),
			allocation.GetAgentId(),
			allocation.GetPhase(),
			allocation.GetRolloutState(),
			allocation.GetHealthy(),
			allocation.GetAppliedSpecRevision(),
			allocation.GetAppliedRolloutGeneration(),
			allocationEndpoint(allocation),
			allocation.GetMessage(),
		))
	}
	return strings.Join(parts, "; ")
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
	if allocation == nil {
		return ""
	}
	if strings.TrimSpace(allocation.GetAllocationIpv4()) != "" && len(allocation.GetHealthyIpv4Ports()) > 0 {
		return net.JoinHostPort(allocation.GetAllocationIpv4(), strconv.Itoa(int(allocation.GetHealthyIpv4Ports()[0])))
	}
	if strings.TrimSpace(allocation.GetAllocationIpv6()) != "" && len(allocation.GetHealthyIpv6Ports()) > 0 {
		return net.JoinHostPort(allocation.GetAllocationIpv6(), strconv.Itoa(int(allocation.GetHealthyIpv6Ports()[0])))
	}
	return ""
}

func allocationEndpoints(allocation *platformv1.AllocationStatus) []string {
	if allocation == nil {
		return nil
	}
	var endpoints []string
	if strings.TrimSpace(allocation.GetAllocationIpv4()) != "" && len(allocation.GetHealthyIpv4Ports()) > 0 {
		endpoints = append(endpoints, net.JoinHostPort(allocation.GetAllocationIpv4(), strconv.Itoa(int(allocation.GetHealthyIpv4Ports()[0]))))
	}
	if strings.TrimSpace(allocation.GetAllocationIpv6()) != "" && len(allocation.GetHealthyIpv6Ports()) > 0 {
		endpoints = append(endpoints, net.JoinHostPort(allocation.GetAllocationIpv6(), strconv.Itoa(int(allocation.GetHealthyIpv6Ports()[0]))))
	}
	return endpoints
}
