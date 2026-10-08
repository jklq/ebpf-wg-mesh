//go:build integration && linux

package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeSystemdInstallRetryKeepsProcessAndRequiresActualReadiness(t *testing.T) {
	if os.Getenv("PRODUCTION_FLEET_INTEGRATION") != "1" {
		t.Skip("set PRODUCTION_FLEET_INTEGRATION=1 with the isolated systemd host image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	image := os.Getenv("PRODUCTION_HOST_IMAGE")
	if image == "" {
		image = "platform-operations-fixture/host:r43"
	}
	name := fmt.Sprintf("native-launch-%d", time.Now().UnixNano())
	command := func(args ...string) []byte {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("native launch command: %v %s", err, b)
		}
		return b
	}
	command("run", "-d", "--name", name, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", image)
	defer exec.Command("docker", "rm", "--force", name).Run()
	i, r, _ := fixture(1)
	i.ID = name
	pl := Placement{Role: Registry, Host: i.Hosts[0].ID, Instance: "registry-launch"}
	program := []byte("#!/bin/sh\nexec /bin/sleep 10000\n")
	role := r.Programs[Registry]
	artifact := role.Artifacts[i.Hosts[0].Architecture]
	artifact.SHA256 = fmt.Sprintf("%x", sha256.Sum256(program))
	role.Artifacts[i.Hosts[0].Architecture] = artifact
	role.Args, role.Ready = nil, []string{"/bin/false"}
	r.Programs[Registry] = role
	p := Plan{Installation: i, Release: r, Placements: []Placement{pl}, Reservations: map[string]Resources{}}
	run := func(script string) error {
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, "sh", "-s")
		cmd.Stdin = strings.NewReader("set -eu\numask 077\n" + script)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		if err != nil {
			t.Logf("native script failed: %s", output.Bytes())
		}
		return err
	}
	if err := run(fileScript(releaseDir(p, pl)+"/program", program, "0755") + "mkdir -p " + quote(stateDir(i, pl)) + "\n"); err != nil {
		t.Fatal(err)
	}
	script, err := installScript(p, pl, i.Hosts[0].Capacity)
	if err != nil || run(script) != nil {
		t.Fatal("native install", err)
	}
	pid := strings.TrimSpace(string(command("exec", name, "systemctl", "show", "--property=MainPID", "--value", unit(i, pl))))
	if pid == "0" || pid == "" {
		t.Fatal("unit never started a process")
	}
	if err := run(script); err != nil {
		t.Fatal("native interrupted-install retry", err)
	}
	retryPID := strings.TrimSpace(string(command("exec", name, "systemctl", "show", "--property=MainPID", "--value", unit(i, pl))))
	if pid != retryPID {
		t.Fatal("retry restarted the actual converging process", pid, retryPID)
	}
	proof, err := installedScript(p, pl, i.Hosts[0].Capacity)
	if err != nil || run(proof) == nil {
		t.Fatal("live process was accepted despite failed native readiness", err)
	}
	role.Ready = []string{"/bin/true"}
	p.Release.Programs[Registry] = role
	// Changing the ready command changes the declared launch input too.
	script, _ = installScript(p, pl, i.Hosts[0].Capacity)
	if err := run(script); err != nil {
		t.Fatal(err)
	}
	proof, _ = installedScript(p, pl, i.Hosts[0].Capacity)
	if err := run(proof); err != nil {
		t.Fatal("native desired process verification", err)
	}
	if err := run(fileScript(releaseDir(p, pl)+"/program", []byte("changed after launch"), "0755")); err != nil {
		t.Fatal(err)
	}
	if err := run(proof); err == nil {
		t.Fatal("changed executable was accepted under a prior launch")
	}
}
