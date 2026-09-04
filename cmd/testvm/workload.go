package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
)

func runWorkloadIsolationChecks(ctx, userCtx context.Context, client platformv1.PlatformServiceClient, sshKeyPath string, hosts map[string]hostInfo, environmentID string, healthyHost hostInfo, healthyContainer string) error {
	infof("scenario: probing production sandbox from a healthy workload")
	probes := []struct {
		name string
		cmd  string
	}{
		{"host sockets", `test ! -e /run/containerd/containerd.sock && test ! -e /var/run/docker.sock`},
		{"host devices", `test ! -e /dev/kmsg && test ! -e /dev/sda && test ! -e /dev/mem`},
		{"masked paths", `for path in /proc/kcore /sys/kernel/security /sys/fs/bpf; do awk -v path="$path" '$5 == path && $0 ~ / - tmpfs / { found=1 } END { exit !found }' /proc/self/mountinfo || exit 1; done`},
		{"image user", `test "$(id -u)" = 0`},
		{"writable overlay root", `touch /overlay-write-probe && rm /overlay-write-probe`},
		{"bounded capabilities", `caps=$(grep '^CapEff:' /proc/self/status | awk '{print $2}'); test $((0x$caps & 0x400)) -ne 0 && test $((0x$caps & 0x200000)) -eq 0 && test $((0x$caps & 0x1000)) -eq 0`},
	}
	for _, probe := range probes {
		execID := fmt.Sprintf("sandbox-%s-%d", strings.ReplaceAll(probe.name, " ", "-"), time.Now().UnixNano())
		remote := fmt.Sprintf("ctr --namespace default task exec --exec-id %q %q sh -c %q", execID, healthyContainer, probe.cmd)
		if _, err := runRemoteCommand(ctx, sshKeyPath, healthyHost.PublicIPv4, remote); err != nil {
			return fmt.Errorf("sandbox probe %s failed: %w", probe.name, err)
		}
	}

	infof("scenario: launching a process-exhaustion and memory-pressure neighbor")
	pressureSpec := inMemoryHTTPServiceSpec("pressure")
	pressureSpec.Runtime.Command = []string{"sh", "-c"}
	pressureSpec.Runtime.Args = []string{":(){ :|:& };:; dd if=/dev/zero of=/tmp/blob bs=1M count=512; sleep 30"}
	pressureSpec.Runtime.MemoryMebibytes = 64
	if _, err := client.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "pressure",
			Spec: pressureSpec,
		},
	}); err != nil {
		return fmt.Errorf("create pressure service: %w", err)
	}
	if _, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		return fmt.Errorf("deploy pressure service: %w", err)
	}
	time.Sleep(5 * time.Second)
	if err := waitForRemoteCommand(ctx, sshKeyPath, healthyHost.PublicIPv4, "systemctl is-active --quiet ebpf-wg-mesh-agent"); err != nil {
		return fmt.Errorf("agent died under tenant pressure: %w", err)
	}
	execID := fmt.Sprintf("neighbor-%d", time.Now().UnixNano())
	if _, err := runRemoteCommand(ctx, sshKeyPath, healthyHost.PublicIPv4, fmt.Sprintf("ctr --namespace default task exec --exec-id %q %q true", execID, healthyContainer)); err != nil {
		return fmt.Errorf("healthy neighbor was lost under tenant pressure: %w", err)
	}
	infof("scenario: noisy-neighbor containment held")
	return nil
}

func volumeBackedHTTPServiceSpec(marker, volumeName string) *platformv1.ServiceSpec {
	return &platformv1.ServiceSpec{
		Runtime: &platformv1.ServiceRuntime{
			Command:         []string{"sh", "-c"},
			Args:            []string{"printf '%s\\n' \"$MARKER\" > /data/index.html && exec httpd -f -p [::]:8080 -h /data"},
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
			Args:            []string{"mkdir -p /tmp/www && printf '%s\\n' \"$MARKER\" > /tmp/www/index.html && exec httpd -f -p 0.0.0.0:8080 -h /tmp/www"},
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

func dualStackHTTPServiceSpec(marker string) *platformv1.ServiceSpec {
	spec := inMemoryHTTPServiceSpec(marker)
	spec.Runtime.Args = []string{"mkdir -p /tmp/www && printf '%s\\n' \"$MARKER\" > /tmp/www/index.html && exec httpd -f -p [::]:8080 -h /tmp/www"}
	return spec
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
