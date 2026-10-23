package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/continuity"
	"ebof-wg-mesh/internal/isolation"
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
	// Preserve native errors on the host without returning secret-bearing stderr
	// to the installer. The script digest also separates nested lifecycle calls.
	digest := sha256.Sum256([]byte(script))
	diagnostic := fmt.Sprintf("/var/log/ebpf-wg-mesh/%s/ssh-%x.stderr", i.ID, digest[:12])
	cmd.Stdin = strings.NewReader("set -eu\numask 077\nmkdir -p " + quote(filepath.Dir(diagnostic)) + "\nexec 2>" + quote(diagnostic) + "\n" + script)
	var stdout limitedBuffer
	cmd.Stdout = &stdout
	// Remote stderr can contain command arguments or secret values. The error
	// identifies the host and exit status; journal inspection stays operator-owned.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("SSH host %s: %w; private diagnostic: %s", h.ID, err, diagnostic)
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
			joins = append(joins, net.JoinHostPort(host.Network.Address, "5432"))
		}
	}
	sort.Strings(joins)
	for name := range p.Release.Tools {
		s = strings.ReplaceAll(s, "{tool."+name+"}", toolsDir(p)+"/"+name)
	}
	return strings.NewReplacer("{installation}", p.Installation.ID, "{release}", p.Release.ID, "{host}", pl.Host, "{instance}", pl.Instance, "{address}", h.Network.Address, "{socketHost}", h.Network.SocketHost(), "{stateDir}", stateDir(p.Installation, pl), "{configDir}", ConfigurationDir(p, pl), "{releaseDir}", releaseDir(p, pl), "{joins}", strings.Join(joins, ","), "{backup}", p.Installation.Backup.Target).Replace(s)
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
		script += "if ! test -f " + quote(dir+"/"+name) + " || ! printf %s " + quote(artifact.SHA256+"  "+dir+"/"+name+"\n") + " | sha256sum -c - >/dev/null 2>&1; then\n"
		limit := ""
		if artifact.Bytes > 0 {
			limit = fmt.Sprintf(" --max-filesize %d", artifact.Bytes)
		}
		script += "curl --fail --silent --location --proto '=https' --proto-redir '=https'" + limit + " --output " + quote(dir+"/"+name+".next") + " -- " + quote(artifact.URL) + "\nprintf %s " + quote(artifact.SHA256+"  "+dir+"/"+name+".next\n") + " | sha256sum -c - >/dev/null\nchmod 0755 " + quote(dir+"/"+name+".next") + "\nmv -f " + quote(dir+"/"+name+".next") + " " + quote(dir+"/"+name) + "\n"
		script += "fi\n"
	}
	return script + fileScript(dir+"/staged", []byte(Digest(p.Release.Tools)), "0600"), nil
}

// gvisorTools are the release tools a host needs to run tenant code behind
// gVisor, the production isolation default for agents and builders: runsc
// and its containerd shim from the same gVisor release, plus runsc-buildkit
// so BuildKit RUN steps execute through runsc on builders.
func gvisorTools(pl Placement) []string {
	switch pl.Role {
	case Agent:
		return []string{"runsc", "containerd-shim-runsc-v1"}
	case Builder:
		return []string{"runsc", "containerd-shim-runsc-v1", "runsc-buildkit"}
	default:
		return nil
	}
}

// gvisorSelected reports whether the placement runs the gVisor isolation
// class. Deployed units run the production profile, whose default class is
// gVisor; only an explicit component environment override selects another.
func gvisorSelected(p Plan, pl Placement) (bool, error) {
	if len(gvisorTools(pl)) == 0 {
		return false, nil
	}
	env := p.Installation.Components[pl.Role].Env
	raw := env["AGENT_ISOLATION"]
	if pl.Role == Builder {
		raw = env["BUILDER_SANDBOX_ISOLATION"]
	}
	class, err := isolation.NormalizeClass(raw, true)
	if err != nil {
		return false, fmt.Errorf("%s isolation selection: %w", pl.Role, err)
	}
	return class == isolation.ClassGVisor, nil
}

// gvisorInstallScript links the staged, checksummed gVisor tools into
// /usr/local/bin, where the systemd default PATH puts them in reach of the
// agent/builder startup checks, containerd's shim discovery, and the
// per-execution BuildKit daemon. A release that does not package them
// fails the install instead of leaving the host to an operator's manual
// install. An explicitly non-gVisor placement needs none of them.
func gvisorInstallScript(p Plan, pl Placement) (string, error) {
	selected, err := gvisorSelected(p, pl)
	if err != nil || !selected {
		return "", err
	}
	h, _ := p.Installation.Host(pl.Host)
	script := ""
	for _, name := range gvisorTools(pl) {
		artifact, ok := p.Release.Tools[name][h.Architecture]
		if !ok {
			return "", fmt.Errorf("release %s must package %s for %s hosts running the gVisor isolation class", p.Release.ID, name, pl.Role)
		}
		link := "/usr/local/bin/" + name
		script += "ln -s " + quote(toolsDir(p)+"/"+name) + " " + quote(link) + `".next.$$"` + "\n"
		script += "mv -f " + quote(link) + `".next.$$"` + " " + quote(link) + "\n"
		script += "test -x " + quote(link) + "\n"
		script += "printf %s " + quote(artifact.SHA256+"  "+link+"\n") + " | sha256sum -c - >/dev/null\n"
	}
	return script, nil
}

// gvisorChecks returns sha256sum -c lines proving the installed gVisor
// tools still resolve to this release's checksummed artifacts.
func gvisorChecks(p Plan, pl Placement) (string, error) {
	selected, err := gvisorSelected(p, pl)
	if err != nil || !selected {
		return "", err
	}
	h, _ := p.Installation.Host(pl.Host)
	checks := ""
	for _, name := range gvisorTools(pl) {
		artifact, ok := p.Release.Tools[name][h.Architecture]
		if !ok {
			return "", fmt.Errorf("release %s must package %s for %s hosts running the gVisor isolation class", p.Release.ID, name, pl.Role)
		}
		checks += artifact.SHA256 + "  /usr/local/bin/" + name + "\n"
	}
	return checks, nil
}

func fileScript(path string, b []byte, mode string) string {
	next := quote(path) + `".next.$$"`
	return "mkdir -p " + quote(path[:strings.LastIndex(path, "/")]) + "\nprintf %s " + quote(base64.StdEncoding.EncodeToString(b)) + " | base64 -d > " + next + "\nchmod " + mode + " " + next + "\nmv -f " + next + " " + quote(path) + "\n"
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
	cfg := ConfigurationDir(p, pl)
	script := "mkdir -p " + quote(dir) + " " + quote(cfg) + " " + quote(stateDir(i, pl)) + "\n"
	for _, name := range i.Components[pl.Role].Storage {
		script += "mkdir -p " + quote(i.Storage[name].Path) + "\n"
	}
	script += "if ! test -f " + quote(dir+"/program") + " || ! printf %s " + quote(artifact.SHA256+"  "+dir+"/program\n") + " | sha256sum -c - >/dev/null 2>&1; then\n"
	limit := ""
	if artifact.Bytes > 0 {
		limit = fmt.Sprintf(" --max-filesize %d", artifact.Bytes)
	}
	script += "curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2" + limit + " --output " + quote(dir+"/program.next") + " -- " + quote(artifact.URL) + "\nprintf %s " + quote(artifact.SHA256+"  "+dir+"/program.next\n") + " | sha256sum -c - >/dev/null\nchmod 0755 " + quote(dir+"/program.next") + "\nmv -f " + quote(dir+"/program.next") + " " + quote(dir+"/program") + "\nfi\n"
	return script, nil
}

// Credentials issued by bootstrap are materialized after the database and
// platform keys exist. Staging executable artifacts never requires those keys
// or overwrites the configuration of a still-running old release.
func configurationScript(p Plan, pl Placement) (string, error) {
	i := p.Installation
	cfg := ConfigurationDir(p, pl)
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
func unitConfiguration(p Plan, pl Placement, observed Resources) (string, string, error) {
	i := p.Installation
	h, _ := i.Host(pl.Host)
	reserve := p.Reservations[pl.Host]
	env := map[string]string{"PLATFORM_INSTALLATION": i.ID, "PLATFORM_INSTANCE": pl.Instance, "PLATFORM_HOST": h.ID, "PLATFORM_CONFIG_DIR": ConfigurationDir(p, pl), "PLATFORM_STATE_DIR": stateDir(i, pl), "PLATFORM_RELEASE": p.Release.ID}
	for key, value := range i.Components[pl.Role].Env {
		env[key] = expand(p, pl, value)
	}
	for key, ref := range i.Components[pl.Role].SecretEnv {
		value, err := p.resolveSecret(pl, ref)
		if err != nil {
			return "", "", err
		}
		v := strings.TrimSpace(string(value))
		if strings.ContainsAny(v, "\n\r") {
			return "", "", fmt.Errorf("secret environment reference %s must be a single line", ref)
		}
		env[key] = v
	}
	if contains([]Role{Agent, ControlPlane, Console, Builder}, pl.Role) {
		prefix := map[Role]string{Agent: "AGENT", ControlPlane: "CONTROLPLANE", Console: "DASHBOARD", Builder: "BUILDER"}[pl.Role]
		env[prefix+"_AUTHORITY_FILE"] = ConfigurationDir(p, pl) + "/authority.json"
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
		env["CONTROLPLANE_SHUTDOWN_WITHDRAWAL_SECONDS"] = "10"
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
		env["DASHBOARD_DRAIN_FILE"] = stateDir(i, pl) + "/draining"
		env["SERVER_SHUTDOWN_TIMEOUT"] = "60"
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
	program := p.Release.Programs[pl.Role]
	argv := append([]string{releaseDir(p, pl) + "/program"}, expandCommand(p, pl, program.Args)...)
	stop := ""
	if len(program.Stop) > 0 {
		stop = "ExecStop=" + systemdCommand(expandCommand(p, pl, program.Stop)) + "\n"
	}
	resources := ""
	if pl.Role == Agent {
		// systemd reapplies resource properties during daemon-reload. Persist
		// the runtime's protections in the unit so installing another component
		// cannot reset the live agent's cgroup to unprotected defaults.
		resources = fmt.Sprintf("MemoryMin=%dM\nMemoryLow=%dM\nCPUWeight=10000\n", reserve.MemoryMiB, reserve.MemoryMiB)
	}
	user := ""
	if pl.Role == Database {
		user = "User=" + DatabaseOSUser + "\n"
	}
	service := "[Unit]\nDescription=Platform " + string(pl.Role) + " " + pl.Instance + "\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=simple\n" + user + "EnvironmentFile=" + ConfigurationDir(p, pl) + "/environment\nEnvironmentFile=-" + ConfigurationDir(p, pl) + "/runtime.env\nEnvironmentFile=" + ConfigurationDir(p, pl) + "/launch.env\nExecStart=" + systemdCommand(argv) + "\n" + stop + "WorkingDirectory=" + stateDir(i, pl) + "\nRestart=always\nRestartSec=5\nTimeoutStopSec=120\nUMask=0077\n" + resources + "[Install]\nWantedBy=multi-user.target\n"
	if len(program.Listeners) > 0 {
		owner := []string{"PLATFORM_CONFIG_DIR=" + configDir(i, pl), "PLATFORM_CONTINUITY_OWNER=" + ownerDigest(p, pl)}
		service = "[Unit]\nDescription=Platform listener owner " + string(pl.Role) + " " + pl.Instance + "\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=simple\nDelegate=yes\nEnvironment=" + systemdCommand(owner) + "\nExecStart=" + systemdCommand([]string{ContinuityBinary(p, pl), "serve", ContinuityDir(p, pl)}) + "\n" + stop + "Restart=always\nRestartSec=1\nTimeoutStopSec=120\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n"
	}
	return environment.String(), service, nil
}

// systemd Exec lines have their own specifier and environment expansion, without a shell.
func systemdCommand(argv []string) string {
	args := make([]string, 0, len(argv))
	for _, arg := range argv {
		args = append(args, "\""+strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(arg)+"\"")
	}
	return strings.Join(args, " ")
}

func launchDigestScript(p Plan, pl Placement) string {
	cfg := ConfigurationDir(p, pl)
	unitPath := "/etc/systemd/system/" + unit(p.Installation, pl)
	return "launch_digest=$( { sha256sum " + quote(cfg+"/environment") + " " + quote(unitPath) + "; if test -f " + quote(cfg+"/runtime.env") + "; then sha256sum " + quote(cfg+"/runtime.env") + "; fi; printf %s " + quote(fingerprint(p, pl)) + "; } | sha256sum | cut -d ' ' -f1)\n"
}
func runningLaunchScript(p Plan, pl Placement) string {
	name := quote(unit(p.Installation, pl))
	return "systemctl is-active --quiet " + name + " && launch_pid=$(systemctl show --property=MainPID --value " + name + ") && test \"$launch_pid\" -gt 1 && tr '\\000' '\\n' < \"/proc/$launch_pid/environ\" | grep -Fxq -- \"PLATFORM_LAUNCH_DIGEST=$launch_digest\""
}
func installScript(p Plan, pl Placement, observed Resources) (string, error) {
	if len(p.Release.Programs[pl.Role].Listeners) > 0 {
		return installContinuityScript(p, pl, observed)
	}
	env, service, err := unitConfiguration(p, pl, observed)
	if err != nil {
		return "", err
	}
	cfg := ConfigurationDir(p, pl)
	unitPath := "/etc/systemd/system/" + unit(p.Installation, pl)
	gvisor, err := gvisorInstallScript(p, pl)
	if err != nil {
		return "", err
	}
	script := "if test -L " + quote(unitPath) + "; then rm " + quote(unitPath) + "; fi\n"
	script += gvisor
	script += fileScript(cfg+"/environment", []byte(env), "0600") + fileScript(unitPath, []byte(service), "0644")
	script += fileScript(cfg+"/replicas", replicaCount(p, pl), "0600")
	script += launchDigestScript(p, pl)
	script += "printf 'PLATFORM_LAUNCH_DIGEST=\"%s\"\\n' \"$launch_digest\" > " + quote(cfg+"/launch.env.next") + "\nchmod 0600 " + quote(cfg+"/launch.env.next") + "\nmv -f " + quote(cfg+"/launch.env.next") + " " + quote(cfg+"/launch.env") + "\nsystemctl daemon-reload\nsystemctl enable " + quote(unit(p.Installation, pl)) + "\n"
	// A slow or interrupted startup must be allowed to converge. Only a process
	// launched with the actual desired inputs can avoid a restart; readiness is
	// still checked independently by observation and production verification.
	script += "if ! ( " + runningLaunchScript(p, pl) + " ); then systemctl restart " + quote(unit(p.Installation, pl)) + "; fi\n"
	return script, nil
}
func installedScript(p Plan, pl Placement, observed Resources) (string, error) {
	if len(p.Release.Programs[pl.Role].Listeners) > 0 {
		return runningOwnerScript(p, pl) + "\n" + command([]string{ContinuityBinary(p, pl), "verify", ContinuityDir(p, pl), ConfigurationDir(p, pl) + "/launch.json"}) + "\n" + command(expandCommand(p, pl, p.Release.Programs[pl.Role].Ready)) + "\n", nil
	}
	env, service, err := unitConfiguration(p, pl, observed)
	if err != nil {
		return "", err
	}
	cfg := ConfigurationDir(p, pl)
	unitPath := "/etc/systemd/system/" + unit(p.Installation, pl)
	h, _ := p.Installation.Host(pl.Host)
	checks := fmt.Sprintf("%x  %s\n%x  %s\n%s  %s\n%x  %s\n", sha256.Sum256([]byte(env)), cfg+"/environment", sha256.Sum256([]byte(service)), unitPath, p.Release.Programs[pl.Role].Artifacts[h.Architecture].SHA256, releaseDir(p, pl)+"/program", sha256.Sum256(replicaCount(p, pl)), cfg+"/replicas")
	gvisor, err := gvisorChecks(p, pl)
	if err != nil {
		return "", err
	}
	checks += gvisor
	return "printf %s " + quote(checks) + " | sha256sum -c - >/dev/null\n" + launchDigestScript(p, pl) + runningLaunchScript(p, pl) + "\n" + command(expandCommand(p, pl, p.Release.Programs[pl.Role].Ready)) + "\n", nil
}

// Keep replica counts outside the launch digest so scaling does not restart existing replicas.
func replicaCount(p Plan, pl Placement) []byte {
	count := 0
	for _, peer := range p.Placements {
		if peer.Role == pl.Role {
			count++
		}
	}
	return []byte(strconv.Itoa(count) + "\n")
}

// Changing listener addresses or the owner executable requires restarting the owner.
func ownerDigest(p Plan, pl Placement) string {
	h, _ := p.Installation.Host(pl.Host)
	return Digest([]any{p.Release.Tools["continuity"][h.Architecture].SHA256, p.Release.Programs[pl.Role].Listeners})[:24]
}
func runningOwnerScript(p Plan, pl Placement) string {
	name := quote(unit(p.Installation, pl))
	return "systemctl is-active --quiet " + name + " && owner_pid=$(systemctl show --property=MainPID --value " + name + ") && test \"$owner_pid\" -gt 1 && tr '\\000' '\\n' < \"/proc/$owner_pid/environ\" | grep -Fxq -- " + quote("PLATFORM_CONTINUITY_OWNER="+ownerDigest(p, pl))
}

func installContinuityScript(p Plan, pl Placement, observed Resources) (string, error) {
	env, service, err := unitConfiguration(p, pl, observed)
	if err != nil {
		return "", err
	}
	cfg := ConfigurationDir(p, pl)
	binary, dir := ContinuityBinary(p, pl), ContinuityDir(p, pl)
	h, _ := p.Installation.Host(pl.Host)
	program, resources := p.Release.Programs[pl.Role], p.Installation.Components[pl.Role].Resources
	l := continuity.Launch{
		Command:          append([]string{releaseDir(p, pl) + "/program"}, expandCommand(p, pl, program.Args)...),
		ConfigDir:        cfg,
		Directory:        stateDir(p.Installation, pl),
		EnvFiles:         []string{cfg + "/environment", cfg + "/runtime.env"},
		Listeners:        program.Listeners,
		Ready:            expandCommand(p, pl, program.Ready),
		Stop:             expandCommand(p, pl, program.Stop),
		MemoryMiB:        resources.MemoryMiB,
		CPUMillis:        resources.CPUMillis,
		EnforceResources: true,
	}
	b, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	unitName, unitPath := quote(unit(p.Installation, pl)), "/etc/systemd/system/"+unit(p.Installation, pl)
	script := "if test -L " + quote(unitPath) + "; then rm " + quote(unitPath) + "; fi\n"
	script += fileScript(cfg+"/environment", []byte(env), "0600") + fileScript(cfg+"/launch.json", b, "0600")
	script += fileScript(configDir(p.Installation, pl)+"/replicas", replicaCount(p, pl), "0600")
	check := "printf %s " + quote(p.Release.Tools["continuity"][h.Architecture].SHA256+"  "+binary+"\n") + " | sha256sum -c - >/dev/null"
	script += "if ! " + check + " 2>&1; then mkdir -p " + quote(filepath.Dir(binary)) + "\ncp " + quote(toolsDir(p)+"/continuity") + " " + quote(binary) + `".next.$$"` + "\nchmod 0755 " + quote(binary) + `".next.$$"` + "\nmv -f " + quote(binary) + `".next.$$"` + " " + quote(binary) + "\nfi\n" + check + "\n"
	script += "if ! ( " + runningOwnerScript(p, pl) + " ); then\nsystemctl stop " + unitName + "\n" + fileScript(unitPath, []byte(service), "0644") + "systemctl daemon-reload\n" + command([]string{binary, "seed", dir, cfg + "/launch.json"}) + "\nelse\n" + fileScript(unitPath, []byte(service), "0644") + "systemctl daemon-reload\nfi\n"
	script += "systemctl enable " + unitName + "\nsystemctl start " + unitName + "\n"
	script += "n=0\nuntil " + command([]string{binary, "status", dir}) + " >/dev/null 2>&1; do n=$((n+1)); test \"$n\" -lt 100; sleep 0.1; done\n"
	script += command([]string{binary, "activate", dir, cfg + "/launch.json"}) + " >/dev/null\n"
	return script, nil
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
