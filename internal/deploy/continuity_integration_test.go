//go:build integration && linux

package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/continuity"
)

func TestNativeSystemdContinuityUpgradeAndResourceEnforcement(t *testing.T) {
	if os.Getenv("PRODUCTION_FLEET_INTEGRATION") != "1" {
		t.Skip("set PRODUCTION_FLEET_INTEGRATION=1 with the isolated systemd host image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binaries := map[string]string{}
	for _, name := range []string{"continuity", "operations", "helper"} {
		path := filepath.Join(dir, name)
		args := []string{"build", "-o", path, "./cmd/" + name}
		if name == "helper" {
			args = []string{"test", "-c", "-o", path, "./internal/continuity"}
		}
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = "../.."
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fixture: %v %s", err, b)
		}
		binaries[name] = path
	}
	name := fmt.Sprintf("continuity-native-%d", time.Now().UnixNano())
	dockerCommand := func(args ...string) []byte {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("isolated native command: %v %s", err, b)
		}
		return b
	}
	image := os.Getenv("PRODUCTION_HOST_IMAGE")
	if image == "" {
		image = "platform-operations-fixture/host:r43"
	}
	dockerCommand("run", "-d", "--name", name, "--network=none", "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", image)
	defer exec.Command("docker", "rm", "--force", name).Run()
	i, r, _ := fixture(1)
	i.ID = name
	i.Release = "r43"
	r.ID = "r43"
	pl := Placement{Role: Registry, Host: "a", Instance: "registry"}
	component := i.Components[Registry]
	component.Resources = Resources{1000, 256, 1}
	component.Env = map[string]string{"CONTINUITY_HELPER": "1", "GENERATION": "old", "DATA_FILE": "{stateDir}/mutations"}
	i.Components[Registry] = component
	r.Tools = map[string]map[string]Artifact{}
	for _, tool := range []string{"continuity", "operations"} {
		b, err := os.ReadFile(binaries[tool])
		if err != nil {
			t.Fatal(err)
		}
		r.Tools[tool] = map[string]Artifact{"amd64": {SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}}
	}
	helper, _ := os.ReadFile(binaries["helper"])
	program := r.Programs[Registry]
	program.Artifacts["amd64"] = Artifact{SHA256: fmt.Sprintf("%x", sha256.Sum256(helper))}
	program.Args = []string{"-test.run=TestContinuityHelperProcess", "--", "serve"}
	program.Listeners = []continuity.Listener{{Address: "127.0.0.1:15000", Env: "HTTP_ADDR"}}
	program.Ready = []string{"{tool.operations}", "ready", "registry", "{instance}", "{configDir}/probe.json"}
	r.Programs[Registry] = program
	p := Plan{Installation: i, Release: r, Placements: []Placement{pl}, Reservations: map[string]Resources{}}
	run := func(script string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, "sh", "-s")
		cmd.Stdin = strings.NewReader("set -eu\numask 077\n" + script)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("native script: %v %s", err, b)
		}
		return b
	}
	for _, tool := range []string{"continuity", "operations"} {
		run("mkdir -p " + quote(toolsDir(p)) + "\n")
		dockerCommand("cp", binaries[tool], name+":"+toolsDir(p)+"/"+tool)
	}
	run("mkdir -p " + quote(releaseDir(p, pl)) + " " + quote(ConfigurationDir(p, pl)) + " " + quote(stateDir(i, pl)) + "\n")
	dockerCommand("cp", binaries["helper"], name+":"+releaseDir(p, pl)+"/program")
	run(fileScript(ConfigurationDir(p, pl)+"/runtime.env", nil, "0600") + fileScript(ConfigurationDir(p, pl)+"/probe.json", []byte(`{"url":"http://127.0.0.1:15000/ready","status":200}`), "0600"))
	script, err := installScript(p, pl, i.Hosts[0].Capacity)
	if err != nil {
		t.Fatal(err)
	}
	run(script)
	owner := strings.TrimSpace(string(run("systemctl show --value -p MainPID " + quote(unit(i, pl)) + "\n")))
	if got := string(dockerCommand("exec", name, "curl", "--silent", "--fail", "http://127.0.0.1:15000/")); got != "old" {
		t.Fatal(got)
	}
	component.Env["GENERATION"] = "new"
	i.Components[Registry] = component
	p.Installation = i
	script, err = installScript(p, pl, i.Hosts[0].Capacity)
	if err != nil {
		t.Fatal(err)
	}
	run(script)
	if got := strings.TrimSpace(string(run("systemctl show --value -p MainPID " + quote(unit(i, pl)) + "\n"))); got != owner {
		t.Fatal("updater restarted the listener owner")
	}
	if got := string(dockerCommand("exec", name, "curl", "--silent", "--fail", "http://127.0.0.1:15000/")); got != "new" {
		t.Fatal(got)
	}
	proof, err := installedScript(p, pl, i.Hosts[0].Capacity)
	if err != nil {
		t.Fatal(err)
	}
	run(proof)
	var status continuity.Status
	b := run(command([]string{ContinuityBinary(p, pl), "status", ContinuityDir(p, pl)}) + "\n")
	if err = json.Unmarshal(bytes.TrimSpace(b), &status); err != nil {
		t.Fatal(err)
	}
	limits := run(fmt.Sprintf("cg=$(sed -n 's/^0:://p' /proc/%d/cgroup)\ncat /sys/fs/cgroup\"$cg\"/memory.max\ncat /sys/fs/cgroup\"$cg\"/cpu.max\n", status.PID))
	if strings.TrimSpace(string(limits)) != "268435456\n100000 100000" {
		t.Fatalf("native limits absent: %s", limits)
	}
	run(command([]string{ContinuityBinary(p, pl), "finish", ContinuityDir(p, pl)}) + "\n")
}
