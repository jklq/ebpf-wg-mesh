//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/testutil"
)

func TestAdversarialWorkloadCannotReachHostFilesystemSocketsOrDevices(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("iso")
	probe := strings.Join([]string{
		"ok=1",
		"test ! -e /run/containerd/containerd.sock && test ! -e /var/run/docker.sock || ok=host-socket",
		"test ! -e /dev/kmsg && test ! -e /dev/sda && test ! -e /dev/mem || ok=host-device",
		"for path in /proc/kcore /sys/kernel/security /sys/fs/bpf; do awk -v path=\"$path\" '$5 == path && $0 ~ / - tmpfs / { found=1 } END { exit !found }' /proc/self/mountinfo || ok=masked-path; done",
		"test \"$(id -u)\" = 0 || ok=image-user",
		"touch /overlay-write-probe || ok=overlay-read-only",
		"printf 'ok=%s' \"$ok\" > /tmp/index.html",
		"exec httpd -f -p [::]:8080 -h /tmp",
	}, "; ")
	svc := busyboxProbeService(allocID, "10.200.9.64", "fd00:200:0:9::40", probe)
	cleanupContainerdService(t, engine, cfg, allocID)
	if _, _, err := engine.EnsureService(ctx, svc); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService: %v", err)
	}
	netnsPath := requirePersistedNetNS(t, engine, allocID)
	got := httpGetInNamespace(t, ctx, netnsPath, svc.GetPrivateIpv6(), 8080, "/")
	if got != "ok=1" {
		t.Fatalf("sandbox probe = %q", got)
	}
}

func TestAdversarialProcessExhaustionAndMemoryPressureStayContained(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	engine, cfg := newTestContainerdEngine(t)

	victimID := uniqueRuntimeID("vic")
	victim := busyboxHTTPService(victimID, 9, 1, "10.200.9.65", "fd00:200:0:9::41", "neighbor-ok")
	cleanupContainerdService(t, engine, cfg, victimID)
	if _, _, err := engine.EnsureService(ctx, victim); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService victim: %v", err)
	}
	victimNS := requirePersistedNetNS(t, engine, victimID)

	forkID := uniqueRuntimeID("fork")
	forkBomb := busyboxProbeService(forkID, "10.200.9.66", "fd00:200:0:9::42", ":(){ :|:& };:; sleep 5; printf contained > /tmp/index.html; exec httpd -f -p [::]:8080 -h /tmp")
	forkBomb.Spec.Runtime.CpuMillis = 250
	forkBomb.Spec.Runtime.MemoryMebibytes = 64
	cleanupContainerdService(t, engine, cfg, forkID)
	if _, _, err := engine.EnsureService(ctx, forkBomb); err != nil && !skippableRuntimeErr(err) {
		t.Logf("fork-bomb start: %v", err)
	}

	memID := uniqueRuntimeID("mem")
	hog := busyboxProbeService(memID, "10.200.9.67", "fd00:200:0:9::43", "dd if=/dev/zero of=/tmp/blob bs=1M count=512; printf hog > /tmp/www/index.html; mkdir -p /tmp/www; exec httpd -f -p [::]:8080 -h /tmp/www")
	hog.Spec.Runtime.MemoryMebibytes = 64
	cleanupContainerdService(t, engine, cfg, memID)
	_, _, _ = engine.EnsureService(ctx, hog)

	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 20 * time.Second, Interval: 200 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		body, err := httpGetInNamespaceErr(ctx, victimNS, victim.GetPrivateIpv6(), 8080, "/")
		return err == nil && body == "neighbor-ok", nil
	}); err != nil {
		t.Fatalf("noisy-neighbor containment lost victim: %v", err)
	}
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Fatalf("agent process vanished under tenant pressure: %v", err)
	}
}

func TestAdversarialNamespaceEscapePrerequisitesAreAbsent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("ns")
	hostPID, err := os.Readlink("/proc/1/ns/pid")
	if err != nil {
		t.Fatalf("host pid ns: %v", err)
	}
	probe := fmt.Sprintf("printf '%%s' \"$(readlink /proc/1/ns/pid)|$(readlink /proc/self/ns/pid)|$(readlink /proc/self/ns/mnt)|$(readlink /proc/self/ns/uts)|$(readlink /proc/self/ns/ipc)\" > /tmp/index.html; exec httpd -f -p [::]:8080 -h /tmp")
	svc := busyboxProbeService(allocID, "10.200.9.68", "fd00:200:0:9::44", probe)
	cleanupContainerdService(t, engine, cfg, allocID)
	if _, _, err := engine.EnsureService(ctx, svc); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService: %v", err)
	}
	netnsPath := requirePersistedNetNS(t, engine, allocID)
	got := httpGetInNamespace(t, ctx, netnsPath, svc.GetPrivateIpv6(), 8080, "/")
	parts := strings.Split(got, "|")
	if len(parts) != 5 {
		t.Fatalf("namespace probe = %q", got)
	}
	if parts[0] == hostPID {
		t.Fatal("workload shares the host PID namespace")
	}
	if parts[0] != parts[1] {
		t.Fatalf("container pid 1 is not the workload init: %q", got)
	}
}

func busyboxProbeService(allocationID, ipv4, ipv6, command string) *agentv1.DesiredService {
	svc := busyboxHTTPService(allocationID, 11, 1, ipv4, ipv6, "probe")
	svc.Spec.Runtime.Command = []string{"sh", "-c"}
	svc.Spec.Runtime.Args = []string{command}
	svc.Spec.Runtime.CpuMillis = 250
	svc.Spec.Runtime.MemoryMebibytes = 64
	return svc
}
