//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"github.com/containerd/containerd/errdefs"
)

func TestEphemeralDiskDefaultLimitIsOneGiB(t *testing.T) {
	t.Parallel()
	if defaultEphemeralDiskLimitBytes != 1<<30 {
		t.Fatalf("defaultEphemeralDiskLimitBytes = %d, want 1 GiB", defaultEphemeralDiskLimitBytes)
	}
	var engine containerdEngine
	if engine.ephemeralDiskLimit() != 1<<30 {
		t.Fatalf("default engine limit = %d, want 1 GiB", engine.ephemeralDiskLimit())
	}
}

func TestContainerdRuntimeStopsAllocationOverEphemeralDiskLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	engine, cfg := newTestContainerdEngine(t)
	engine.(*containerdEngine).ephemeralDiskLimitBytes = 16 << 20
	allocID := uniqueRuntimeID("disk")
	svc := diskHogService(allocID, 21, 1, "10.200.9.70", "fd00:200:0:9::50", 48)
	state := &agentv1.DesiredNodeState{
		AgentId:              "node-1",
		ReconciliationCursor: 1,
		Services:             []*agentv1.DesiredService{svc},
	}

	cleanupContainerdService(t, engine, cfg, allocID)
	runtime := newDiskTestRuntime(t, cfg, engine)
	var cond *agentv1.ServiceCondition
	lastLog := ""
	for deadline := time.Now().Add(200 * time.Second); ; {
		cond = reconcileOnceForDiskTest(t, ctx, runtime, state)
		if summary := cond.GetPhase() + "|" + cond.GetRestart().GetLastCause().String(); summary != lastLog {
			t.Logf("allocation %s: %s", summary, cond.GetMessage())
			lastLog = summary
		}
		if cond.GetPhase() == "CrashLoop" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("over-quota allocation did not crash loop: %+v", cond)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if cond.GetRestart().GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED {
		t.Fatalf("last cause = %s", cond.GetRestart().GetLastCause())
	}
	if !strings.Contains(strings.ToLower(cond.GetMessage()), "disk exhausted") {
		t.Fatalf("message %q does not name disk exhaustion", cond.GetMessage())
	}
	ce := engine.(*containerdEngine)
	if _, exists, err := ce.inspect(ce.namespaced(ctx), containerName(allocID)); err != nil || exists {
		t.Fatalf("over-quota container still present: exists=%t err=%v", exists, err)
	}
	requireSnapshotCleaned(t, ctx, engine, cfg, allocID)
	requireHostDiskNotExhausted(t)
}

func TestContainerdRuntimeStopsWriterPastOneGiB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("disk1g")
	svc := diskHogService(allocID, 22, 1, "10.200.9.71", "fd00:200:0:9::51", 1127)
	state := &agentv1.DesiredNodeState{
		AgentId:              "node-1",
		ReconciliationCursor: 1,
		Services:             []*agentv1.DesiredService{svc},
	}

	cleanupContainerdService(t, engine, cfg, allocID)
	runtime := newDiskTestRuntime(t, cfg, engine)
	var cond *agentv1.ServiceCondition
	lastLog := ""
	for deadline := time.Now().Add(150 * time.Second); ; {
		cond = reconcileOnceForDiskTest(t, ctx, runtime, state)
		if summary := cond.GetPhase() + "|" + cond.GetRestart().GetLastCause().String(); summary != lastLog {
			t.Logf("allocation %s: %s", summary, cond.GetMessage())
			lastLog = summary
		}
		if cond.GetRestart().GetLastCause() == platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("1 GiB writer was not stopped: %+v", cond)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(strings.ToLower(cond.GetMessage()), "disk exhausted") {
		t.Fatalf("message %q does not name disk exhaustion", cond.GetMessage())
	}
	ce := engine.(*containerdEngine)
	if _, exists, err := ce.inspect(ce.namespaced(ctx), containerName(allocID)); err != nil || exists {
		t.Fatalf("1 GiB writer still present: exists=%t err=%v", exists, err)
	}
	requireSnapshotCleaned(t, ctx, engine, cfg, allocID)
	requireHostDiskNotExhausted(t)
}

func diskHogService(allocationID string, identity uint32, generation int64, ipv4, ipv6 string, megabytes int) *agentv1.DesiredService {
	svc := busyboxHTTPService(allocationID, identity, generation, ipv4, ipv6, "hog")
	svc.Spec.Runtime.Command = []string{"sh", "-c"}
	svc.Spec.Runtime.Args = []string{fmt.Sprintf(
		"mkdir -p /disk-hog && dd if=/dev/zero of=/disk-hog/blob bs=1M count=%d && exec httpd -f -p [::]:8080 -h /tmp",
		megabytes,
	)}
	svc.Spec.Runtime.CpuMillis = 500
	svc.Spec.Runtime.MemoryMebibytes = 1024
	return svc
}

func newDiskTestRuntime(t *testing.T, cfg config.AgentConfig, engine serviceEngine) *ContainerdRuntime {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.DataDir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Runtime.VolumesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &ContainerdRuntime{cfg: cfg, engine: engine}
}

func reconcileOnceForDiskTest(t *testing.T, ctx context.Context, runtime *ContainerdRuntime, state *agentv1.DesiredNodeState) *agentv1.ServiceCondition {
	t.Helper()
	report, err := runtime.Reconcile(ctx, state)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.GetServices()) != 1 {
		t.Fatalf("services = %d, want 1", len(report.GetServices()))
	}
	cond := report.GetServices()[0]
	if cond.GetPhase() == "Error" && skippableRuntimeErr(errors.New(cond.GetMessage())) {
		t.Skipf("containerd/CNI cannot start a workload: %s", cond.GetMessage())
	}
	return cond
}

func requireSnapshotCleaned(t *testing.T, ctx context.Context, engine serviceEngine, cfg config.AgentConfig, allocationID string) {
	t.Helper()
	ce := engine.(*containerdEngine)
	_, err := ce.client.SnapshotService(cfg.Runtime.Snapshotter).Usage(ce.namespaced(ctx), containerName(allocationID))
	if err == nil {
		t.Fatal("over-quota snapshot still consumes host disk")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("snapshot usage: %v", err)
	}
}

func requireHostDiskNotExhausted(t *testing.T) {
	t.Helper()
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		t.Fatalf("statfs /: %v", err)
	}
	if free := stat.Bavail * uint64(stat.Bsize); free < 256<<20 {
		t.Fatalf("host disk nearly exhausted: %d bytes free", free)
	}
}
