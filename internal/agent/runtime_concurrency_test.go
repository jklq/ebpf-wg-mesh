package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type blockedAllocationEngine struct {
	fakeEngine
	started  chan string
	release  chan struct{}
	blockAll bool
	active   atomic.Int32
	maximum  atomic.Int32
}

func (e *blockedAllocationEngine) EnsureService(ctx context.Context, svc *agentv1.DesiredService) (serviceStatus, bool, error) {
	active := e.active.Add(1)
	defer e.active.Add(-1)
	for previous := e.maximum.Load(); active > previous; previous = e.maximum.Load() {
		if e.maximum.CompareAndSwap(previous, active) {
			break
		}
	}
	e.started <- svc.GetAllocationId()
	if e.blockAll || svc.GetAllocationId() == "slow" {
		select {
		case <-e.release:
		case <-ctx.Done():
			return serviceStatus{}, false, ctx.Err()
		}
	}
	return serviceStatus{AppliedSpecRevision: svc.GetDesiredSpecRevision(), AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(), Running: true}, false, nil
}

func concurrentRuntime(t *testing.T, engine serviceEngine) *ContainerdRuntime {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &ContainerdRuntime{cfg: config.AgentConfig{Runtime: config.RuntimeConfig{DataDir: dir, VolumesDir: filepath.Join(dir, "volumes")}}, engine: engine}
}

func TestAllocationConcurrencyIsBounded(t *testing.T) {
	engine := &blockedAllocationEngine{started: make(chan string, 8), release: make(chan struct{}), blockAll: true}
	runtime := concurrentRuntime(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Reconcile(ctx, testDesiredState(1, 1, "a", "b", "c", "d", "e", "f"))
		done <- err
	}()
	for range allocationReconcileConcurrency {
		select {
		case <-engine.started:
		case <-ctx.Done():
			t.Fatal("workers did not start concurrently")
		}
	}
	select {
	case id := <-engine.started:
		t.Fatalf("worker limit exceeded by %s", id)
	case <-time.After(50 * time.Millisecond):
	}
	close(engine.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if engine.maximum.Load() != allocationReconcileConcurrency {
		t.Fatalf("max workers = %d", engine.maximum.Load())
	}
}

func TestSupervisorPublishesProgressWhileSharedVolumeGroupWaits(t *testing.T) {
	engine := &blockedAllocationEngine{started: make(chan string, 8), release: make(chan struct{})}
	runtime := concurrentRuntime(t, engine)
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	state := testDesiredState(1, 1, "slow", "slow-next", "fast")
	for _, svc := range state.Services[:2] {
		svc.VolumeId = "volume-1"
		svc.Spec.Runtime.Volume = &platformv1.ServiceVolumeMount{VolumeName: "data", MountPath: "/data"}
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", state); err != nil {
		t.Fatal(err)
	}
	supervisor := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { supervisor.reconcile(ctx, "test"); close(done) }()
	select {
	case <-supervisor.ReportNotifications():
	case <-ctx.Done():
		t.Fatal("slow allocation blocked reporting")
	}
	report, err := supervisor.CurrentReport()
	if err != nil {
		t.Fatal(err)
	}
	conditions := make(map[string]*agentv1.ServiceCondition)
	for _, condition := range report.GetServices() {
		conditions[condition.GetAllocationId()] = condition
	}
	if len(report.GetServices()) != 3 || !conditions["fast"].GetHealthy() || conditions["slow"].GetPhase() != "Pending" {
		t.Fatalf("partial inventory = %v", report)
	}
	for range 2 {
		select {
		case id := <-engine.started:
			if id == "slow-next" {
				t.Fatal("shared volume lost desired order")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(engine.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("pass did not complete")
	}
	if id := <-engine.started; id != "slow-next" {
		t.Fatalf("next shared allocation = %s", id)
	}
}

func TestHealthFamiliesProbeConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		probeHealthFamilies(ctx, func(ctx context.Context, _ string, ip string, _ *agentv1.DesiredService) serviceHealthProbe {
			started <- ip
			select {
			case <-release:
			case <-ctx.Done():
			}
			return serviceHealthProbe{healthy: true}
		}, serviceStatus{AllocationIPv4: "v4", AllocationIPv6: "v6"}, nil)
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("one family waited for the other probe")
		}
	}
	close(release)
	<-done
}

func TestDiskScanDoesNotBlockCachedReadsAndRemovalDiscardsItsResult(t *testing.T) {
	var cache diskUsageCache
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		cache.usage("allocation", time.Now(), func() (int64, error) { close(started); <-release; return 100, nil })
		close(done)
	}()
	<-started
	read := make(chan int64, 1)
	go func() { read <- cache.cached("allocation") }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("disk walk blocked lifecycle inspection")
	}
	cache.remove("allocation")
	close(release)
	<-done
	if cache.cached("allocation") != 0 {
		t.Fatal("removed snapshot's late result was retained")
	}
}
