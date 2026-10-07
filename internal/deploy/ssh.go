package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Remote interface {
	Run(context.Context, Installation, Host, string) ([]byte, error)
	Upload(context.Context, Installation, Host, string, string, string) error
}
type SSHRemote struct{}

func sshCommand(ctx context.Context, i Installation, h Host, command string) (*exec.Cmd, error) {
	if _, err := i.Resolve(h.SSH.Key); err != nil {
		return nil, err
	}
	address := h.SSH.Address
	port := "22"
	if host, p, err := net.SplitHostPort(address); err == nil {
		address, port = host, p
	}
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + h.SSH.KnownHosts, "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-i", i.Secrets[h.SSH.Key].File, "-p", port, "--", h.SSH.User + "@" + address}
	if h.SSH.User != "root" {
		command = "sudo -n " + command
	}
	return exec.CommandContext(ctx, "ssh", append(args, command)...), nil
}

func (SSHRemote) Run(ctx context.Context, i Installation, h Host, script string) ([]byte, error) {
	cmd, err := sshCommand(ctx, i, h, "sh -s")
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader("set -eu\numask 077\n" + script)
	var stdout limitedBuffer
	cmd.Stdout = &stdout
	// Remote stderr can contain command arguments or secret values. The error
	// identifies the host and exit status; journal inspection stays operator-owned.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("SSH host %s: %w", h.ID, err)
	}
	return stdout.Bytes(), nil
}

// Upload streams a protected executable through SSH, checks its release digest
// on the destination, and atomically installs it without a download service.
func (SSHRemote) Upload(ctx context.Context, i Installation, h Host, local, target, digest string) error {
	input, err := os.Open(local)
	if err != nil {
		return err
	}
	defer input.Close()
	next := target + ".next"
	script := "set -eu\numask 077\nmkdir -p " + quote(filepath.Dir(target)) + "\ntrap " + quote("rm -f "+quote(next)) + " EXIT\ncat > " + quote(next) + "\nprintf %s " + quote(digest+"  "+next+"\n") + " | sha256sum -c - >/dev/null\nchmod 0755 " + quote(next) + "\nmv -f " + quote(next) + " " + quote(target) + "\n"
	cmd, err := sshCommand(ctx, i, h, "sh -c "+quote(script))
	if err != nil {
		return err
	}
	cmd.Stdin = input
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("SSH protected artifact upload to host %s failed: %w", h.ID, err)
	}
	return nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 1<<20 {
		return 0, fmt.Errorf("remote output exceeds 1 MiB")
	}
	_, err := b.Buffer.Write(p)
	return n, err
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func command(argv []string) string {
	q := make([]string, len(argv))
	for n, a := range argv {
		q[n] = quote(a)
	}
	return strings.Join(q, " ")
}
func unit(i Installation, pl Placement) string {
	return "platform-" + i.ID + "-" + pl.Instance + ".service"
}
func releaseDir(p Plan, pl Placement) string {
	return "/opt/ebpf-wg-mesh/" + p.Installation.ID + "/" + p.Release.ID + "/" + pl.Instance
}
func configDir(i Installation, pl Placement) string {
	return "/etc/ebpf-wg-mesh/" + i.ID + "/" + pl.Instance
}
func stateDir(i Installation, pl Placement) string {
	return "/var/lib/ebpf-wg-mesh/" + i.ID + "/" + pl.Instance
}
func expand(p Plan, pl Placement, s string) string {
	h, _ := p.Installation.Host(pl.Host)
	var joins []string
	for _, db := range p.Placements {
		if db.Role == Database {
			host, _ := p.Installation.Host(db.Host)
			joins = append(joins, net.JoinHostPort(host.Network.Address, "26257"))
		}
	}
	sort.Strings(joins)
	for name := range p.Release.Tools {
		s = strings.ReplaceAll(s, "{tool."+name+"}", toolsDir(p)+"/"+name)
	}
	return strings.NewReplacer("{installation}", p.Installation.ID, "{release}", p.Release.ID, "{host}", pl.Host, "{instance}", pl.Instance, "{address}", h.Network.Address, "{stateDir}", stateDir(p.Installation, pl), "{configDir}", configDir(p.Installation, pl), "{releaseDir}", releaseDir(p, pl), "{joins}", strings.Join(joins, ","), "{backup}", p.Installation.Backup.Target).Replace(s)
}
func expandCommand(p Plan, pl Placement, args []string) []string {
	out := make([]string, len(args))
	for n, a := range args {
		out[n] = expand(p, pl, a)
	}
	return out
}

func (p Plan) resolveSecret(pl Placement, ref string) ([]byte, error) {
	s, ok := p.Installation.Secrets[ref]
	if !ok {
		return nil, fmt.Errorf("unknown secret reference %q", ref)
	}
	return readSecretFile(ref, expand(p, pl, s.File))
}

func toolsDir(p Plan) string {
	return "/opt/ebpf-wg-mesh/" + p.Installation.ID + "/" + p.Release.ID + "/tools"
}
func toolsScript(p Plan, h Host) (string, error) {
	dir := toolsDir(p)
	script := "mkdir -p " + quote(dir) + "\n"
	names := make([]string, 0, len(p.Release.Tools))
	for name := range p.Release.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		artifact, ok := p.Release.Tools[name][h.Architecture]
		if !ok {
			return "", fmt.Errorf("management tool %s unavailable for %s", name, h.Architecture)
		}
		script += "curl --fail --silent --location --proto '=https' --proto-redir '=https' --output " + quote(dir+"/"+name+".next") + " -- " + quote(artifact.URL) + "\nprintf %s " + quote(artifact.SHA256+"  "+dir+"/"+name+".next\n") + " | sha256sum -c - >/dev/null\nchmod 0755 " + quote(dir+"/"+name+".next") + "\nmv -f " + quote(dir+"/"+name+".next") + " " + quote(dir+"/"+name) + "\n"
	}
	return script + fileScript(dir+"/staged", []byte(Digest(p.Release.Tools)), "0600"), nil
}
func fileScript(path string, b []byte, mode string) string {
	return "mkdir -p " + quote(path[:strings.LastIndex(path, "/")]) + "\nprintf %s " + quote(base64.StdEncoding.EncodeToString(b)) + " | base64 -d > " + quote(path+".next") + "\nchmod " + mode + " " + quote(path+".next") + "\nmv -f " + quote(path+".next") + " " + quote(path) + "\n"
}
func stageScript(p Plan, pl Placement) (string, error) {
	i := p.Installation
	h, ok := i.Host(pl.Host)
	if !ok {
		return "", fmt.Errorf("placement host %s absent", pl.Host)
	}
	prog := p.Release.Programs[pl.Role]
	artifact, ok := prog.Artifacts[h.Architecture]
	if !ok {
		return "", fmt.Errorf("missing artifact for %s/%s", pl.Role, h.Architecture)
	}
	dir := releaseDir(p, pl)
	cfg := configDir(i, pl)
	script := "mkdir -p " + quote(dir) + " " + quote(cfg) + " " + quote(stateDir(i, pl)) + "\n"
	for _, name := range i.Components[pl.Role].Storage {
		script += "mkdir -p " + quote(i.Storage[name].Path) + "\n"
	}
	script += "if ! test -f " + quote(dir+"/program") + " || ! printf %s " + quote(artifact.SHA256+"  "+dir+"/program\n") + " | sha256sum -c - >/dev/null 2>&1; then\n"
	script += "curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --output " + quote(dir+"/program.next") + " -- " + quote(artifact.URL) + "\nprintf %s " + quote(artifact.SHA256+"  "+dir+"/program.next\n") + " | sha256sum -c - >/dev/null\nchmod 0755 " + quote(dir+"/program.next") + "\nmv -f " + quote(dir+"/program.next") + " " + quote(dir+"/program") + "\nfi\n"
	return script, nil
}

// Credentials issued by bootstrap are materialized after the database and
// platform keys exist. Staging executable artifacts never requires those keys
// or overwrites the configuration of a still-running old release.
func configurationScript(p Plan, pl Placement) (string, error) {
	i := p.Installation
	cfg := configDir(i, pl)
	script := "mkdir -p " + quote(cfg) + "\n"
	paths := make([]string, 0, len(i.Components[pl.Role].Secrets))
	for path := range i.Components[pl.Role].Secrets {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		value, err := p.resolveSecret(pl, i.Components[pl.Role].Secrets[path])
		if err != nil {
			return "", err
		}
		script += fileScript(cfg+"/"+path, value, "0600")
	}
	return script, nil
}
func installScript(p Plan, pl Placement, observed Resources) (string, error) {
	i := p.Installation
	h, _ := i.Host(pl.Host)
	reserve := p.Reservations[pl.Host]
	env := map[string]string{"PLATFORM_INSTALLATION": i.ID, "PLATFORM_INSTANCE": pl.Instance, "PLATFORM_HOST": h.ID, "PLATFORM_CONFIG_DIR": configDir(i, pl), "PLATFORM_STATE_DIR": stateDir(i, pl), "PLATFORM_RELEASE": p.Release.ID}
	for key, value := range i.Components[pl.Role].Env {
		env[key] = expand(p, pl, value)
	}
	for key, ref := range i.Components[pl.Role].SecretEnv {
		value, err := p.resolveSecret(pl, ref)
		if err != nil {
			return "", err
		}
		v := strings.TrimSpace(string(value))
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("secret environment reference %s must be a single line", ref)
		}
		env[key] = v
	}
	if contains([]Role{Agent, ControlPlane, Console, Builder}, pl.Role) {
		prefix := map[Role]string{Agent: "AGENT", ControlPlane: "CONTROLPLANE", Console: "DASHBOARD", Builder: "BUILDER"}[pl.Role]
		env[prefix+"_AUTHORITY_FILE"] = configDir(i, pl) + "/authority.json"
	}
	if pl.Role == Agent {
		env["AGENT_NODE_ID"] = pl.Instance
		env["AGENT_PROFILE"] = "production"
		env["AGENT_CPU_MILLIS"] = strconv.FormatInt(min(h.Capacity.CPUMillis, observed.CPUMillis), 10)
		env["AGENT_MEMORY_MEBIBYTES"] = strconv.FormatInt(min(h.Capacity.MemoryMiB, observed.MemoryMiB), 10)
		env["AGENT_RESERVED_CPU_MILLIS"] = strconv.FormatInt(reserve.CPUMillis, 10)
		env["AGENT_RESERVED_MEMORY_MEBIBYTES"] = strconv.FormatInt(reserve.MemoryMiB, 10)
	}
	if pl.Role == Builder {
		// The builder's own budget is available for its admitted build. Reserve
		// only the host's other consumers when computing build admission.
		builderReserve := reserve.Sub(i.Components[Builder].Resources)
		env["BUILDER_PROFILE"] = "production"
		env["BUILDER_HOST_TYPE"] = "stable"
		if h.Reliability == "intermittent" {
			env["BUILDER_HOST_TYPE"] = "intermittent"
		}
		env["BUILDER_RESERVE_CPU_MILLIS"] = strconv.FormatInt(builderReserve.CPUMillis, 10)
		env["BUILDER_RESERVE_MEMORY_BYTES"] = strconv.FormatInt(builderReserve.MemoryMiB<<20, 10)
	}
	if pl.Role == ControlPlane {
		env["CONTROLPLANE_PROFILE"] = "production"
		env["CONTROLPLANE_DASHBOARD_ENABLED"] = "false"
		var consoleIDs []string
		for _, console := range p.Placements {
			if console.Role == Console {
				consoleIDs = append(consoleIDs, console.Instance)
			}
		}
		sort.Strings(consoleIDs)
		env["CONTROLPLANE_CONSOLE_CALLER_IDS"] = strings.Join(consoleIDs, ",")
	}
	if pl.Role == Console {
		env["DASHBOARD_PROFILE"] = "production"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var environment strings.Builder
	for _, k := range keys {
		environment.WriteString(k + "=\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(env[k]) + "\"\n")
	}
	argv := append([]string{releaseDir(p, pl) + "/program"}, expandCommand(p, pl, p.Release.Programs[pl.Role].Args)...)
	// systemd does not use a shell for ExecStart. Escape its own specifier and
	// environment expansion as well as quoted argument delimiters.
	var execArgs []string
	for _, arg := range argv {
		execArgs = append(execArgs, "\""+strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(arg)+"\"")
	}
	service := "[Unit]\nDescription=Platform " + string(pl.Role) + " " + pl.Instance + "\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=simple\nEnvironmentFile=" + configDir(i, pl) + "/environment\nExecStart=" + strings.Join(execArgs, " ") + "\nWorkingDirectory=" + stateDir(i, pl) + "\nRestart=always\nRestartSec=5\nTimeoutStopSec=120\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n"
	return fileScript(configDir(i, pl)+"/environment", []byte(environment.String()), "0600") + fileScript("/etc/systemd/system/"+unit(i, pl), []byte(service), "0644") + "systemctl daemon-reload\nsystemctl enable " + quote(unit(i, pl)) + "\nsystemctl restart " + quote(unit(i, pl)) + "\n" + fileScript(configDir(i, pl)+"/applied", []byte(fingerprint(p, pl)), "0600"), nil
}
func fingerprint(p Plan, pl Placement) string {
	secrets := map[string]string{}
	for key, ref := range p.Installation.Components[pl.Role].SecretEnv {
		value, err := p.resolveSecret(pl, ref)
		if err != nil {
			secrets[key] = "unavailable"
		} else {
			secrets[key] = Digest(value)
		}
	}
	h, _ := p.Installation.Host(pl.Host)
	var peers []Placement
	for _, peer := range p.Placements {
		if peer.Role == Database || (pl.Role == ControlPlane && peer.Role == Console) {
			peers = append(peers, peer)
		}
	}
	reserve := Resources{}
	if pl.Role == Agent || pl.Role == Builder {
		reserve = p.Reservations[pl.Host]
	}
	return Digest([]any{p.Release.ID, p.Release.Configuration, p.Release.Protocol, p.Release.Schema, p.Release.ConsoleSchema, p.Release.Programs[pl.Role], p.Installation.Components[pl.Role], pl, h.Network.Address, h.Capacity, reserve, peers, secrets})
}
