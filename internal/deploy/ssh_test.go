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
	pattern := regexp.MustCompile(`printf %s '([A-Za-z0-9+/=]+)' \| base64 -d > '[^']+/environment.next'`)
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
