//go:build linux

package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	containerdapparmor "github.com/containerd/containerd/contrib/apparmor"
	"github.com/containerd/containerd/namespaces"
	hostapparmor "github.com/containerd/containerd/pkg/apparmor"
	cnetns "github.com/containerd/containerd/pkg/netns"
	"github.com/containerd/errdefs"
	cni "github.com/containerd/go-cni"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	buildSandboxLabelExecutor = "ebpf-wg-mesh.build.executor"
	buildSandboxLabelBuildID  = "ebpf-wg-mesh.build.id"
	buildSandboxLabelStep     = "ebpf-wg-mesh.build.step"
	buildSandboxLabelOwnerPID = "ebpf-wg-mesh.build.owner-pid"

	buildSandboxExecutorValue = "hardened"
	buildSandboxAppArmorName  = "ebpf-wg-mesh-build"

	sandboxNetnsDirName     = "netns"
	sandboxDiskPollInterval = 2 * time.Second
	sandboxKillTimeout      = 30 * time.Second
	sandboxKillPollInterval = 200 * time.Millisecond
)

// containerdSandboxBackend is the Linux SandboxBackend: one-shot OCI
// containers on a private network namespace with CNI egress and
// denied-CIDR blackholes. Every setup step fails closed: a build
// whose sandbox cannot be fully contained does not run.
type containerdSandboxBackend struct {
	cfg      SandboxBackendConfig
	client   *containerd.Client
	image    containerd.Image
	netnsDir string
	apparmor bool

	cniMu     sync.Mutex
	cniClient cni.CNI
}

func newSandboxBackendPlatform(cfg SandboxBackendConfig) (SandboxBackend, error) {
	for field, value := range map[string]string{
		"socket":      cfg.Socket,
		"namespace":   cfg.Namespace,
		"runtime":     cfg.Runtime,
		"snapshotter": cfg.Snapshotter,
		"work dir":    cfg.WorkDir,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("sandbox %s is required for the hardened executor", field)
		}
	}
	netnsDir := filepath.Join(cfg.WorkDir, sandboxNetnsDirName)
	if err := os.MkdirAll(netnsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox netns dir: %w", err)
	}
	client, err := containerd.New(cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial containerd: %w", err)
	}
	backend := &containerdSandboxBackend{cfg: cfg, client: client, netnsDir: netnsDir}
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), cfg.Namespace), 30*time.Second)
	defer cancel()
	if _, err := client.Version(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("containerd version check: %w", err)
	}
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("sandbox image %q is not present: pre-pull the operator toolchain image: %w", cfg.Image, err)
	}
	// Unpack for the configured snapshotter so a present-but-unpacked
	// image fails here with a clear error instead of mid-build.
	if err := image.Unpack(ctx, cfg.Snapshotter); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("unpack sandbox image %q for %s: %w", cfg.Image, cfg.Snapshotter, err)
	}
	backend.image = image
	if hostapparmor.HostSupports() {
		if err := containerdapparmor.LoadDefaultProfile(buildSandboxAppArmorName); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("load sandbox AppArmor profile: %w", err)
		}
		backend.apparmor = true
	}
	return backend, nil
}

func (b *containerdSandboxBackend) namespaced(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, b.cfg.Namespace)
}

func (b *containerdSandboxBackend) Close() error {
	if b == nil || b.client == nil {
		return nil
	}
	return b.client.Close()
}

// sandboxNetOwner tracks a network namespace created by SetupNet so a
// crashed builder's namespaces are reaped by owner death, the same
// rule as workspace markers.
type sandboxNetOwner struct {
	Executor  string    `json:"executor"`
	PID       int       `json:"pid"`
	BuildID   string    `json:"build_id"`
	NetNSPath string    `json:"netns_path"`
	StartedAt time.Time `json:"started_at"`
}

func (b *containerdSandboxBackend) netOwnerPath(buildID string) (string, error) {
	id, err := sandboxContainerID(buildID, "ns")
	if err != nil {
		return "", err
	}
	return filepath.Join(b.netnsDir, id+".owner.json"), nil
}

func (b *containerdSandboxBackend) SetupNet(ctx context.Context, buildID string, policy NetworkPolicy) (SandboxNet, error) {
	denied, err := deniedRoutePrefixes(policy)
	if err != nil {
		return SandboxNet{}, err
	}
	ownerPath, err := b.netOwnerPath(buildID)
	if err != nil {
		return SandboxNet{}, err
	}
	// A leftover namespace from a crashed run must never be reused.
	b.removeNetNSForOwner(ownerPath)
	ns, err := cnetns.NewNetNS(b.netnsDir)
	if err != nil {
		return SandboxNet{}, fmt.Errorf("create sandbox netns: %w", err)
	}
	nsPath := ns.GetPath()
	owner := sandboxNetOwner{
		Executor:  ExecutorHardened,
		PID:       os.Getpid(),
		BuildID:   buildID,
		NetNSPath: nsPath,
		StartedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(owner)
	if err != nil {
		cnetns.LoadNetNS(nsPath).Remove()
		return SandboxNet{}, fmt.Errorf("marshal sandbox netns owner: %w", err)
	}
	if err := os.WriteFile(ownerPath, data, 0o644); err != nil {
		cnetns.LoadNetNS(nsPath).Remove()
		return SandboxNet{}, fmt.Errorf("write sandbox netns owner: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			b.removeNetNSForOwner(ownerPath)
		}
	}()

	if !policy.AllowGeneralEgress {
		// Loopback-only: a private namespace with no attachment and
		// no routes. There is nothing to deny because there is no
		// egress path at all.
		if err := inSandboxNetNS(nsPath, bringLoopbackUp); err != nil {
			return SandboxNet{}, fmt.Errorf("isolate sandbox netns: %w", err)
		}
		failed = false
		return SandboxNet{BuildID: buildID, Path: nsPath}, nil
	}

	cniClient, err := b.ensureCNIClient()
	if err != nil {
		return SandboxNet{}, err
	}
	cniID, err := sandboxContainerID(buildID, "net")
	if err != nil {
		return SandboxNet{}, err
	}
	cniOpts := []cni.NamespaceOpts{cni.WithArgs("IgnoreUnknown", "1")}
	result, err := cniClient.Setup(ctx, cniID, nsPath, cniOpts...)
	if err != nil {
		return SandboxNet{}, fmt.Errorf("attach sandbox network: %w", err)
	}
	if !cniResultHasIP(result) {
		_ = cniClient.Remove(ctx, cniID, nsPath, cniOpts...)
		return SandboxNet{}, errors.New("sandbox CNI network assigned no address: refusing host-network fallback")
	}
	if err := inSandboxNetNS(nsPath, func() error {
		if err := bringLoopbackUp(); err != nil {
			return err
		}
		return addDeniedRoutes(denied)
	}); err != nil {
		_ = cniClient.Remove(ctx, cniID, nsPath, cniOpts...)
		return SandboxNet{}, fmt.Errorf("enforce sandbox egress policy: %w", err)
	}
	failed = false
	return SandboxNet{BuildID: buildID, Path: nsPath, Attached: true}, nil
}

func (b *containerdSandboxBackend) TeardownNet(ctx context.Context, sandboxNet SandboxNet) error {
	var errs []error
	if sandboxNet.Attached {
		cniID, idErr := sandboxContainerID(sandboxNet.BuildID, "net")
		if idErr != nil {
			errs = append(errs, idErr)
		} else if cniClient, err := b.ensureCNIClient(); err != nil {
			errs = append(errs, err)
		} else if err := cniClient.Remove(ctx, cniID, sandboxNet.Path, cni.WithArgs("IgnoreUnknown", "1")); err != nil &&
			!os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
			errs = append(errs, fmt.Errorf("detach sandbox network: %w", err))
		}
	}
	ownerPath, err := b.netOwnerPath(sandboxNet.BuildID)
	if err != nil {
		errs = append(errs, err)
	} else {
		b.removeNetNSForOwner(ownerPath)
		if sandboxNet.Path != "" {
			_ = cnetns.LoadNetNS(sandboxNet.Path).Remove()
		}
	}
	return errors.Join(errs...)
}

// removeNetNSForOwner removes the namespace recorded in an owner file
// plus the file itself. It is best effort: teardown and stale
// recovery must not fail on already-removed state.
func (b *containerdSandboxBackend) removeNetNSForOwner(ownerPath string) {
	data, err := os.ReadFile(ownerPath)
	if err == nil {
		var owner sandboxNetOwner
		if err := json.Unmarshal(data, &owner); err == nil && owner.NetNSPath != "" {
			_ = cnetns.LoadNetNS(owner.NetNSPath).Remove()
		}
	}
	_ = os.Remove(ownerPath)
}

func (b *containerdSandboxBackend) ensureCNIClient() (cni.CNI, error) {
	b.cniMu.Lock()
	defer b.cniMu.Unlock()
	if b.cniClient != nil {
		return b.cniClient, nil
	}
	confPath, isList, err := selectCNIConfig(b.cfg.CNIConfDir, b.cfg.CNINetwork)
	if err != nil {
		return nil, err
	}
	client, err := cni.New(
		cni.WithPluginDir([]string{b.cfg.CNIPluginDir}),
		cni.WithPluginConfDir(b.cfg.CNIConfDir),
		cni.WithInterfacePrefix("eth"),
	)
	if err != nil {
		return nil, fmt.Errorf("create sandbox CNI client: %w", err)
	}
	loadOpts := []cni.Opt{cni.WithLoNetwork}
	if isList {
		loadOpts = append(loadOpts, cni.WithConfListFile(confPath))
	} else {
		loadOpts = append(loadOpts, cni.WithConfFile(confPath))
	}
	if err := client.Load(loadOpts...); err != nil {
		return nil, fmt.Errorf("load sandbox CNI network: %w", err)
	}
	b.cniClient = client
	return client, nil
}

func cniResultHasIP(result *cni.Result) bool {
	if result == nil {
		return false
	}
	for _, iface := range result.Interfaces {
		if len(iface.IPConfigs) > 0 {
			return true
		}
	}
	return false
}

// inSandboxNetNS runs fn with the calling thread inside the network
// namespace at nsPath, then restores the host namespace. A thread
// that cannot be restored is deliberately leaked rather than
// returned to the pool in the wrong namespace.
func inSandboxNetNS(nsPath string, fn func() error) (err error) {
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("capture host network namespace: %w", err)
	}
	defer hostNS.Close()
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("open sandbox network namespace: %w", err)
	}
	defer targetNS.Close()

	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()
	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("enter sandbox network namespace: %w", err)
	}
	defer func() {
		if restoreErr := netns.Set(hostNS); restoreErr != nil {
			unlockThread = false
			err = errors.Join(err, fmt.Errorf("restore host network namespace: %w", restoreErr))
		}
	}()
	return fn()
}

func bringLoopbackUp() error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find sandbox loopback: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring sandbox loopback up: %w", err)
	}
	return nil
}

// addDeniedRoutes installs blackhole routes for the denied CIDRs.
// Blackholes drop matching packets silently inside the namespace, die
// with the namespace (no rule teardown to forget), and cannot be
// removed by the sandbox, which holds no capabilities.
func addDeniedRoutes(denied []netip.Prefix) error {
	for _, prefix := range denied {
		masked := prefix.Masked()
		ipNet := &net.IPNet{
			IP:   net.IP(masked.Addr().AsSlice()),
			Mask: net.CIDRMask(masked.Bits(), masked.Addr().BitLen()),
		}
		route := &netlink.Route{Dst: ipNet, Type: unix.RTN_BLACKHOLE, Table: unix.RT_TABLE_MAIN}
		if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, unix.EEXIST) && !os.IsExist(err) {
			return fmt.Errorf("blackhole %s: %w", masked, err)
		}
	}
	return nil
}

func sandboxLabels(buildID, step string) map[string]string {
	return map[string]string{
		buildSandboxLabelExecutor: buildSandboxExecutorValue,
		buildSandboxLabelBuildID:  buildID,
		buildSandboxLabelStep:     step,
		buildSandboxLabelOwnerPID: strconv.Itoa(os.Getpid()),
	}
}

func (b *containerdSandboxBackend) RunStep(ctx context.Context, sandboxNet SandboxNet, step SandboxStep) error {
	if err := validateSandboxMounts(step.Mounts); err != nil {
		return err
	}
	containerID, err := sandboxContainerID(sandboxNet.BuildID, step.Name)
	if err != nil {
		return err
	}
	nctx := b.namespaced(ctx)
	// A leftover container from a crashed step must never be reused.
	_ = b.removeSandboxContainer(nctx, containerID)

	opts, err := buildSandboxSpecOpts(sandboxSpecInput{
		Argv:      step.Argv,
		Env:       step.Env,
		Dir:       step.Dir,
		NetNSPath: sandboxNet.Path,
		Limits:    step.Limits,
		Mounts:    step.Mounts,
	})
	if err != nil {
		return err
	}
	if b.apparmor {
		opts = append(opts, containerdapparmor.WithDefaultProfile(buildSandboxAppArmorName))
	}
	container, err := b.client.NewContainer(nctx, containerID,
		containerd.WithRuntime(b.cfg.Runtime, nil),
		containerd.WithSnapshotter(b.cfg.Snapshotter),
		containerd.WithNewSnapshot(containerID, b.image),
		containerd.WithNewSpec(opts...),
		containerd.WithContainerLabels(sandboxLabels(sandboxNet.BuildID, step.Name)),
	)
	if err != nil {
		return fmt.Errorf("create sandbox container: %w", err)
	}
	defer func() {
		_ = b.removeSandboxContainer(b.namespaced(context.Background()), containerID)
	}()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create sandbox stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("create sandbox stderr pipe: %w", err)
	}
	var (
		combined boundedTailBuffer
		wg       sync.WaitGroup
	)
	combined.max = commandFailureOutputBytes
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanCommandStream(ctx, stdoutR, "stdout", &combined, step.OnLog)
	}()
	go func() {
		defer wg.Done()
		scanCommandStream(ctx, stderrR, "stderr", &combined, step.OnLog)
	}()
	finishIO := func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		wg.Wait()
		_ = stdoutR.Close()
		_ = stderrR.Close()
	}

	task, err := container.NewTask(nctx, cio.NewCreator(cio.WithStreams(nil, stdoutW, stderrW)))
	if err != nil {
		finishIO()
		return fmt.Errorf("create sandbox task: %w", err)
	}
	exitCh, err := task.Wait(nctx)
	if err != nil {
		finishIO()
		return fmt.Errorf("wait for sandbox task: %w", err)
	}
	if err := task.Start(nctx); err != nil {
		finishIO()
		return fmt.Errorf("start sandbox task: %w", err)
	}

	timer := time.NewTicker(sandboxDiskPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			b.killSandboxTask(task, containerID)
			finishIO()
			return ctx.Err()
		case <-timer.C:
			written, err := b.sandboxOverlayBytes(nctx, containerID)
			if err != nil {
				slog.Warn("measure sandbox disk usage", "container_id", containerID, "error", err)
				continue
			}
			if written > step.Limits.MaxWorkspaceBytes {
				b.killSandboxTask(task, containerID)
				finishIO()
				return &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("sandbox wrote %d bytes, exceeding the %d byte limit", written, step.Limits.MaxWorkspaceBytes)}
			}
		case <-exitCh:
			status, err := task.Status(b.namespaced(context.Background()))
			finishIO()
			if err != nil {
				return fmt.Errorf("inspect sandbox task: %w", err)
			}
			if status.ExitStatus != 0 {
				return &SandboxStepError{Step: step.Name, ExitCode: int(status.ExitStatus), Tail: string(combined.Bytes())}
			}
			return nil
		}
	}
}

// killSandboxTask kills a sandbox task and waits for its exit with a
// bound. The kill and the status checks run on a fresh context:
// killing over the execution context after cancellation would fail
// (a cancelled context fails the Kill RPC), and the wait channel of a
// cancelled wait may never deliver, so an unbounded receive would
// stall the worker past its timeout. Container removal stays in the
// caller's deferred cleanup, which reaps the task either way.
func (b *containerdSandboxBackend) killSandboxTask(task containerd.Task, containerID string) {
	killCtx, killCancel := context.WithTimeout(context.Background(), sandboxKillTimeout)
	defer killCancel()
	_ = task.Kill(b.namespaced(killCtx), syscall.SIGKILL)
	deadline := time.Now().Add(sandboxKillTimeout)
	for {
		status, err := task.Status(b.namespaced(killCtx))
		if err == nil && status.Status != containerd.Running {
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("sandbox task did not exit after SIGKILL; container removal will reap it", "container_id", containerID)
			return
		}
		select {
		case <-killCtx.Done():
			return
		case <-time.After(sandboxKillPollInterval):
		}
	}
}

// sandboxOverlayBytes measures bytes written to the sandbox root
// filesystem, excluding the base image.
func (b *containerdSandboxBackend) sandboxOverlayBytes(ctx context.Context, containerID string) (int64, error) {
	snapshots := b.client.SnapshotService(b.cfg.Snapshotter)
	usage, err := snapshots.Usage(ctx, containerID)
	if err != nil {
		return 0, err
	}
	if b.cfg.Snapshotter == "native" {
		if info, err := snapshots.Stat(ctx, containerID); err == nil && info.Parent != "" {
			if parent, err := snapshots.Usage(ctx, info.Parent); err == nil {
				usage.Size -= parent.Size
			}
		}
	}
	if usage.Size < 0 {
		return 0, nil
	}
	return usage.Size, nil
}

// removeSandboxContainer kills and deletes a sandbox container and its
// snapshot. Missing containers are success: removal is idempotent.
func (b *containerdSandboxBackend) removeSandboxContainer(ctx context.Context, containerID string) error {
	container, err := b.client.LoadContainer(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if task, err := container.Task(ctx, nil); err == nil {
		_ = task.Kill(ctx, syscall.SIGKILL)
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (b *containerdSandboxBackend) ReapStale(ctx context.Context) (int, error) {
	nctx := b.namespaced(ctx)
	var (
		reclaimed int
		failures  []string
	)
	records, err := b.client.ContainerService().List(nctx)
	if err != nil {
		return 0, fmt.Errorf("list sandbox containers: %w", err)
	}
	for _, record := range records {
		if record.Labels[buildSandboxLabelExecutor] != buildSandboxExecutorValue {
			continue
		}
		ownerPID, err := strconv.Atoi(strings.TrimSpace(record.Labels[buildSandboxLabelOwnerPID]))
		if err == nil && processAlive(ownerPID) {
			continue
		}
		if err := b.removeSandboxContainer(nctx, record.ID); err != nil {
			failures = append(failures, record.ID+": "+err.Error())
			continue
		}
		reclaimed++
		slog.Info("reclaimed stale sandbox container", "container_id", record.ID)
	}

	entries, err := os.ReadDir(b.netnsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reclaimed, errors.Join(failureList(failures))
		}
		return reclaimed, errors.Join(fmt.Errorf("list sandbox netns dir: %w", err), failureList(failures))
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".owner.json") {
			continue
		}
		ownerPath := filepath.Join(b.netnsDir, entry.Name())
		data, err := os.ReadFile(ownerPath)
		if err != nil {
			failures = append(failures, ownerPath+": "+err.Error())
			continue
		}
		var owner sandboxNetOwner
		if err := json.Unmarshal(data, &owner); err != nil {
			slog.Warn("read stale sandbox netns owner", "path", ownerPath, "error", err)
			continue
		}
		if processAlive(owner.PID) {
			continue
		}
		b.removeNetNSForOwner(ownerPath)
		reclaimed++
		slog.Info("reclaimed stale sandbox netns", "build_id", owner.BuildID)
	}
	return reclaimed, errors.Join(failureList(failures))
}

func failureList(failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("reclaim stale sandboxes: %s", strings.Join(failures, "; "))
}
