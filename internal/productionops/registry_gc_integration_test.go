//go:build integration

package productionops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"github.com/google/uuid"
)

type registryGCSystemdRemote struct {
	container    string
	services     []string
	collections  int
	failShutdown bool
	failStop     bool
}

func (r *registryGCSystemdRemote) Run(ctx context.Context, _ deploy.Installation, _ deploy.Host, script string) ([]byte, error) {
	if r.failShutdown && strings.HasPrefix(script, "if test \"$(systemctl show --value -p LoadState") {
		return nil, errors.New("connection lost before collector shutdown confirmation")
	}
	if strings.Contains(script, "systemctl start ") && strings.Contains(script, "-gc.service") {
		for _, service := range r.services {
			check := "test \"$(systemctl show --value -p ActiveState " + shell(service) + ")\" = inactive"
			if b, err := exec.CommandContext(ctx, "docker", "exec", r.container, "sh", "-eu", "-c", check).CombinedOutput(); err != nil {
				return b, fmt.Errorf("GC overlapped a live registry: %w", err)
			}
		}
		r.collections++
	}
	b, err := exec.CommandContext(ctx, "docker", "exec", r.container, "sh", "-eu", "-c", script).CombinedOutput()
	if err != nil {
		logs, _ := exec.CommandContext(ctx, "docker", "exec", r.container, "journalctl", "--no-pager", "-n", "30").CombinedOutput()
		b = append(b, logs...)
		return b, fmt.Errorf("systemd registry operation: %w: %s", err, b)
	}
	if r.failStop && strings.Contains(script, "MainPID") {
		r.failStop = false
		return nil, errors.New("connection lost after registry stop")
	}
	return b, nil
}

func (*registryGCSystemdRemote) Upload(context.Context, deploy.Installation, deploy.Host, string, string, string) error {
	return errors.New("unexpected executable upload")
}

func TestRegistryGCStopsEveryWriterAndRetriesUncertainShutdown(t *testing.T) {
	if os.Getenv("REGISTRY_GC_SYSTEMD_TEST") != "1" {
		t.Skip("set REGISTRY_GC_SYSTEMD_TEST=1 with the production systemd host image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := "registry-gc-" + uuid.NewString()[:8]
	run := func(args ...string) []byte {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", args[0], err, b)
		}
		return b
	}
	image := os.Getenv("PRODUCTION_HOST_IMAGE")
	if image == "" {
		image = "platform-operations-fixture/host:r43"
	}
	run("run", "-d", "--name", id, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", image)
	t.Cleanup(func() { exec.Command("docker", "rm", "--force", id).Run() })
	copyID := id + "-binary"
	run("create", "--name", copyID, "registry:2")
	t.Cleanup(func() { exec.Command("docker", "rm", "--force", copyID).Run() })
	binary := filepath.Join(t.TempDir(), "registry")
	run("cp", copyID+":/bin/registry", binary)
	run("cp", binary, id+":/tmp/registry")
	for {
		if exec.CommandContext(ctx, "docker", "exec", id, "systemctl", "list-jobs").Run() == nil {
			break
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	host := deploy.Host{ID: "a", Binding: deploy.Binding{Provider: "linux", ServerID: id}}
	pls := []deploy.Placement{{Role: deploy.Registry, Host: "a", Instance: "registry-a"}, {Role: deploy.Registry, Host: "a", Instance: "registry-b"}}
	remote := &registryGCSystemdRemote{container: id}
	r := Runner{Plan: deploy.Plan{ID: "plan", Release: deploy.Release{ID: "r43"}, Installation: deploy.Installation{ID: id, Hosts: []deploy.Host{host}, Providers: map[string]deploy.Provider{"linux": {Kind: "linux"}}}, Placements: pls}, Remote: remote}
	r.defaults()
	for index, pl := range pls {
		remote.services = append(remote.services, unit(r.Plan, pl))
		config := fmt.Sprintf("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: /tmp/registry-data\n  delete:\n    enabled: true\nhttp:\n  addr: 127.0.0.1:%d\n", 15000+index)
		program := "/opt/ebpf-wg-mesh/" + id + "/r43/" + pl.Instance + "/program"
		service := "[Service]\nExecStart=" + program + " serve " + cfgDir(r.Plan, pl) + "/registry.yaml\nRestart=always\n"
		script := remoteFile(cfgDir(r.Plan, pl)+"/registry.yaml", []byte(config)) + remoteFile("/etc/systemd/system/"+unit(r.Plan, pl), []byte(service))
		script += "mkdir -p " + shell(filepath.Dir(program)) + " /tmp/registry-data/docker/registry/v2/repositories /tmp/registry-data/docker/registry/v2/blobs\ncp /tmp/registry " + shell(program) + "\nchmod 0700 " + shell(program) + "\nsystemctl daemon-reload\nsystemctl start " + shell(unit(r.Plan, pl)) + "\n"
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			t.Fatal(err)
		}
	}
	available, err := r.collectRegistryOffline(ctx, pls)
	if err != nil || !available || remote.collections != 1 {
		t.Fatalf("collection: available=%t count=%d err=%v", available, remote.collections, err)
	}
	if err := r.verifyRegistryGC(ctx); err != nil {
		t.Fatal(err)
	}
	// A delayed start from an interrupted caller must remain fenced once writers
	// resume, even though the collector's runtime unit still exists.
	collector := shell(r.registryGCUnit(pls[0]))
	run("exec", id, "sh", "-eu", "-c", "systemctl start "+collector+"\ntest \"$(systemctl show --value -p ConditionResult "+collector+")\" = no\n")
	remote.failStop = true
	available, err = r.collectRegistryOffline(ctx, pls)
	if err == nil || !available || remote.collections != 1 {
		t.Fatalf("ambiguous writer stop: available=%t count=%d err=%v", available, remote.collections, err)
	}
	remote.failShutdown = true
	available, err = r.collectRegistryOffline(ctx, pls)
	if err == nil || available {
		t.Fatal("uncertain collector shutdown restored uploads", err)
	}
	for _, pl := range pls {
		script := "systemctl start " + shell(unit(r.Plan, pl)) + "\n! systemctl is-active --quiet " + shell(unit(r.Plan, pl)) + "\ntest -f " + shell(r.registryGCMarker(pl)) + "\n"
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			t.Fatal("writer gate did not survive an interruption", err)
		}
	}
	remote.failShutdown = false
	available, err = r.collectRegistryOffline(ctx, pls)
	if err != nil || !available || remote.collections != 3 {
		t.Fatalf("retry: available=%t count=%d err=%v", available, remote.collections, err)
	}
	if err := r.verifyRegistryGC(ctx); err != nil {
		t.Fatal(err)
	}
}
