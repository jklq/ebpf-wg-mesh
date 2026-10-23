package deploy

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func installedEnvironment(t *testing.T, script string) string {
	t.Helper()
	pattern := regexp.MustCompile(`printf %s '([A-Za-z0-9+/=]+)' \| base64 -d > '[^']+/environment'"\.next\.\$\$"`)
	match := pattern.FindStringSubmatch(script)
	if len(match) != 2 {
		t.Fatal("missing installed environment")
	}
	data, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallerPinsArtifactsProtectsSecretsAndInjectsReservations(t *testing.T) {
	i, r, inv := fixture(1)
	secret := filepath.Join(t.TempDir(), "wildcard.key")
	value := "private key '$() `do not execute`\n"
	if err := os.WriteFile(secret, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	i.Secrets["wildcard"] = SecretRef{File: secret}
	c := i.Components[ControlPlane]
	c.Secrets = map[string]string{"wildcard.key": "wildcard"}
	i.Components[ControlPlane] = c
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	var cp, agent Placement
	for _, pl := range p.Placements {
		if pl.Role == ControlPlane {
			cp = pl
		}
		if pl.Role == Agent {
			agent = pl
		}
	}
	script, err := stageScript(p, cp)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := configurationScript(p, cp)
	if err != nil {
		t.Fatal(err)
	}
	script += configuration
	if strings.Contains(script, value) || !strings.Contains(script, "sha256sum -c") || !strings.Contains(script, "--proto-redir '=https'") || !strings.Contains(script, "chmod 0600") {
		t.Fatal("artifact/secret protections missing")
	}
	observed := i.Hosts[0].Capacity
	observed.CPUMillis /= 2
	observed.MemoryMiB /= 2
	service, err := installScript(p, agent, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(service, "systemctl enable") || !strings.Contains(service, "systemctl restart") {
		t.Fatal("unit is not independently managed")
	}
	environment := installedEnvironment(t, service)
	if !strings.Contains(environment, `AGENT_MEMORY_MEBIBYTES="`+strconv.FormatInt(observed.MemoryMiB, 10)+`"`) {
		t.Fatal("agent advertised more than the observed physical capacity")
	}
	if !strings.Contains(environment, `AGENT_RESERVED_MEMORY_MEBIBYTES="`+strconv.FormatInt(p.Reservations[agent.Host].MemoryMiB, 10)+`"`) {
		t.Fatal("platform memory not excluded from advertised workload capacity")
	}
	for _, pl := range p.Placements {
		if pl.Role == Builder {
			service, err := installScript(p, pl, observed)
			if err != nil {
				t.Fatal(err)
			}
			reserve := p.Reservations[pl.Host].Sub(i.Components[Builder].Resources)
			if !strings.Contains(installedEnvironment(t, service), `BUILDER_RESERVE_MEMORY_BYTES="`+strconv.FormatInt(reserve.MemoryMiB<<20, 10)+`"`) {
				t.Fatal("builder admission consumed its own declared budget")
			}
		}
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := configurationScript(p, cp); err == nil {
		t.Fatal("world-readable secret accepted")
	}
}
func TestInstallerProvisionsGVisorRuntimeFromReleaseTools(t *testing.T) {
	i, r, inv := fixture(1)
	p := build(t, i, r, State{}, inv, false)
	observed := i.Hosts[0].Capacity
	placements := map[Role]Placement{}
	for _, pl := range p.Placements {
		placements[pl.Role] = pl
	}

	agent, err := installScript(p, placements[Agent], observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runsc", "containerd-shim-runsc-v1"} {
		if !strings.Contains(agent, "ln -s '"+toolsDir(p)+"/"+name+"'") || !strings.Contains(agent, "/usr/local/bin/"+name) {
			t.Fatalf("agent install does not provision %s:\n%s", name, agent)
		}
	}
	if strings.Contains(agent, "runsc-buildkit") {
		t.Fatal("agent install provisions the builder-only runsc-buildkit")
	}
	builder, err := installScript(p, placements[Builder], observed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(builder, "/usr/local/bin/runsc-buildkit") {
		t.Fatalf("builder install does not provision runsc-buildkit:\n%s", builder)
	}
	proof, err := installedScript(p, placements[Agent], observed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proof, "/usr/local/bin/runsc") || !strings.Contains(proof, "/usr/local/bin/containerd-shim-runsc-v1") {
		t.Fatalf("installed proof does not checksum the gVisor tools:\n%s", proof)
	}
	control, err := installScript(p, placements[ControlPlane], observed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(control, "/usr/local/bin/runsc") {
		t.Fatal("controlplane install provisions tenant runtime tools")
	}

	// A release that does not package the runtime fails closed at install
	// instead of relying on a manual host install.
	delete(p.Release.Tools, "runsc")
	if _, err := installScript(p, placements[Agent], observed); err == nil || !strings.Contains(err.Error(), "must package runsc") {
		t.Fatalf("missing runsc tool accepted: %v", err)
	}

	// An explicit non-gVisor class needs no gVisor tools.
	agentComponent := p.Installation.Components[Agent]
	agentComponent.Env = map[string]string{"AGENT_ISOLATION": "runc-userns"}
	p.Installation.Components[Agent] = agentComponent
	script, err := installScript(p, placements[Agent], observed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "/usr/local/bin/runsc") {
		t.Fatal("runc-userns agent install provisions gVisor tools")
	}
}

func TestManifestRejectsUnknownFieldsMultipleDocumentsAndMissingRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nunknownField: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load[Installation](path); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := os.WriteFile(path, []byte("version: 1\n---\nversion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load[Installation](path); err == nil {
		t.Fatal("second document accepted")
	}
	i, r, _ := fixture(1)
	i.Recovery.Hosts = nil
	if err := i.Validate(r); err == nil {
		t.Fatal("no independent recovery inventory")
	}
}
func TestSSHQuoteDoesNotExecuteShellMetacharacters(t *testing.T) {
	value := "don't run $(touch /tmp/not-a-command) `commands`"
	q := quote(value)
	if !strings.HasPrefix(q, "'") || !strings.Contains(q, "'\"'\"'") {
		t.Fatal(q)
	}
}

func TestNativeAddressSubstitutionsPreserveBareIdentityAndIPv6Sockets(t *testing.T) {
	i, r, _ := fixture(1)
	i.Hosts[0].Network.Address = "fd42::a"
	p := Plan{Installation: i, Release: r, Placements: []Placement{{Role: Database, Host: "a"}}}
	pl := Placement{Role: Database, Host: "a", Instance: "database-a"}
	actual := expand(p, pl, "https://{socketHost}:8080/health --listen={socketHost}:5432 --identity={address} --join={joins}")
	if actual != "https://[fd42::a]:8080/health --listen=[fd42::a]:5432 --identity=fd42::a --join=[fd42::a]:5432" {
		t.Fatal(actual)
	}
	i.Hosts[0].Network.Address = "192.0.2.1"
	p.Installation = i
	if actual := expand(p, pl, "{socketHost}:8080"); actual != "192.0.2.1:8080" {
		t.Fatal(actual)
	}
}

func TestUnitsDrainThroughReleaseStopPreparation(t *testing.T) {
	i, r, inv := fixture(1)
	envoy := r.Programs[Envoy]
	envoy.Stop = []string{"/opt/platformops", "prepare-stop", "envoy", "--withdrawal-max=30s", "100%$HOME"}
	r.Programs[Envoy] = envoy
	console := r.Programs[Console]
	console.Stop = []string{"/opt/platformops", "prepare-stop", "console", "{stateDir}/draining"}
	r.Programs[Console] = console
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	units := map[Role]string{}
	environments := map[Role]string{}
	for _, pl := range p.Placements {
		env, service, err := unitConfiguration(p, pl, i.Hosts[0].Capacity)
		if err != nil {
			t.Fatal(err)
		}
		units[pl.Role], environments[pl.Role] = service, env
	}
	stop := regexp.MustCompile(`(?m)^ExecStop=(.*)$`).FindStringSubmatch(units[Envoy])
	if len(stop) != 2 || !strings.Contains(stop[1], `"prepare-stop" "envoy" "--withdrawal-max=30s" "100%%$$HOME"`) {
		t.Fatalf("Envoy unit does not drain before SIGTERM:\n%s", units[Envoy])
	}
	for _, pl := range p.Placements {
		if pl.Role == Console && !strings.Contains(units[Console], `"`+stateDir(i, pl)+`/draining"`) {
			t.Fatalf("console stop preparation does not name its state directory:\n%s", units[Console])
		}
		if pl.Role == Console && !strings.Contains(environments[Console], `DASHBOARD_DRAIN_FILE="`+stateDir(i, pl)+`/draining"`) {
			t.Fatal("console does not read the drain file its stop preparation writes")
		}
	}
	if strings.Contains(units[ControlPlane], "ExecStop=") || !strings.Contains(environments[ControlPlane], `CONTROLPLANE_SHUTDOWN_WITHDRAWAL_SECONDS="10"`) {
		t.Fatal("control plane must drain itself on SIGTERM with a production withdrawal period")
	}
	r.Programs[Envoy] = Program{Artifacts: envoy.Artifacts, Ready: envoy.Ready, Drain: envoy.Drain, Retire: envoy.Retire, Stop: []string{" "}}
	if err := i.Validate(r); err == nil {
		t.Fatal("stop preparation without an executable accepted")
	}
}
