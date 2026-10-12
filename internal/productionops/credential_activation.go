package productionops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

const credentialDigestVariable = "PLATFORM_CREDENTIALS_DIGEST"

const credentialDrainKey = "credential-activation"

var errActivationPending = errors.New("credential activation pending")

// Only launch credentials trigger restarts; runtime identities reload independently.
func launchCredential(role deploy.Role, name string) bool {
	switch name {
	case "probe.json", "probe-ca.crt", "probe-client.crt", "probe-client.key":
		return false
	}
	switch role {
	case deploy.Envoy:
		return !strings.HasPrefix(name, "ingress/") && name != "client.crt" && name != "client.key" && name != "ca.crt"
	case deploy.Agent:
		return !strings.HasPrefix(name, "tls/")
	case deploy.Builder:
		return name != "client.crt" && name != "client.key"
	}
	return true
}

func credentialRuntimeEnvironment(role deploy.Role, files map[string][]byte, env map[string]string) []byte {
	if env == nil {
		env = map[string]string{}
	}
	launch := map[string][]byte{}
	for name, data := range files {
		if launchCredential(role, name) {
			launch[name] = data
		}
	}
	delete(env, credentialDigestVariable)
	env[credentialDigestVariable] = deploy.Digest([]any{launch, environment(env)})
	return environment(env)
}

func credentialIdentityScript(p deploy.Plan, pl deploy.Placement) string {
	return "credential_digest=$(sed -n 's/^" + credentialDigestVariable + "=\"\\([a-f0-9]*\\)\"$/\\1/p' " + shell(cfgDir(p, pl)+"/runtime.env") + ")\ntest \"${#credential_digest}\" = 64\n" +
		"pid=$(systemctl show --value --property=MainPID " + shell(unit(p, pl)) + ")\ntest \"$pid\" -gt 1\n"
}

func credentialEnvironmentCheck() string {
	return "tr '\\000' '\\n' < /proc/\"$pid\"/environ | grep -Fxq -- \"" + credentialDigestVariable + "=$credential_digest\""
}

func credentialActivationScript(p deploy.Plan, pl deploy.Placement, restart bool) string {
	service, directory := shell(unit(p, pl)), cfgDir(p, pl)
	script := "if ! systemctl is-active --quiet " + service + "; then printf 'inactive\\n'; exit 0; fi\n"
	if len(p.Release.Programs[pl.Role].Listeners) > 0 {
		binary, dir := shell(deploy.ContinuityBinary(p, pl)), shell(deploy.ContinuityDir(p, pl))
		launch := shell(directory + "/launch.json")
		return script + "if " + binary + " verify " + dir + " " + launch + " >/dev/null 2>&1; then printf 'current\\n'; exit 0; fi\n" + binary + " activate " + dir + " " + launch + " >/dev/null\n" + binary + " finish " + dir + " >/dev/null\nprintf 'handed-over\\n'\n"
	}
	script += credentialIdentityScript(p, pl)
	if pl.Role == deploy.Database {
		// SIGHUP does not replace the environment. A receipt is valid only for the
		// same native process invocation and is written after readiness succeeds.
		script += "invocation=$(systemctl show --value --property=InvocationID " + service + ")\ntest -n \"$invocation\"\n"
		receipt := directory + "/credentials-loaded"
		script += "loaded=''\nif test -f " + shell(receipt) + "; then loaded=$(cat " + shell(receipt) + "); fi\n"
		script += "case \"$loaded\" in\n\"$invocation/$credential_digest\") printf 'current\\n'; exit 0;;\n\"$invocation/\"*) ;;\n*) if " + credentialEnvironmentCheck() + "; then printf 'current\\n'; exit 0; fi;;\nesac\n"
		// Once a reload is attempted, the inherited environment no longer proves
		// what is loaded, including when an interrupted rotation is rolled back.
		script += "printf '%s/pending\\n' \"$invocation\" > " + shell(receipt+".next") + "\nchmod 0600 " + shell(receipt+".next") + "\nmv -f " + shell(receipt+".next") + " " + shell(receipt) + "\n"
		script += "systemctl kill --kill-who=main --signal=HUP " + service + "\nprintf 'reloaded %s/%s\\n' \"$invocation\" \"$credential_digest\"\n"
	} else {
		script += "if " + credentialEnvironmentCheck() + "; then printf 'current\\n'; exit 0; fi\n"
		if !restart {
			return script + "printf 'stale\\n'\n"
		}
		script += "systemctl restart " + service + "\nprintf 'restarted\\n'\n"
	}
	return script
}

func (r *Runner) activateCredentials(ctx context.Context, pl deploy.Placement) (returnErr error) {
	output, err := r.remote(ctx, r.Plan, pl, credentialActivationScript(r.Plan, pl, false))
	if err != nil {
		return fmt.Errorf("activate credentials for %s: %w", pl.Instance, err)
	}
	status := strings.Fields(string(output))
	if len(status) == 1 && status[0] == "stale" && pl.Role != deploy.Database {
		resume, err := r.prepareCredentialRestart(ctx, pl)
		if err != nil {
			return err
		}
		defer func() {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			returnErr = errors.Join(returnErr, resume(resumeCtx))
		}()
		if output, err = r.remote(ctx, r.Plan, pl, credentialActivationScript(r.Plan, pl, true)); err != nil {
			return fmt.Errorf("activate credentials for %s: %w", pl.Instance, err)
		}
		status = strings.Fields(string(output))
	}
	if len(status) == 1 && status[0] == "inactive" {
		return nil // Maintenance never resurrects a stopped or retired unit.
	}
	if !(len(status) == 1 && (status[0] == "current" || status[0] == "restarted" || status[0] == "handed-over") || len(status) == 2 && status[0] == "reloaded" && pl.Role == deploy.Database) {
		return fmt.Errorf("invalid credential activation result for %s", pl.Instance)
	}
	probe, ok := r.Config.Probes[pl.Role]
	if !ok {
		return fmt.Errorf("missing readiness probe for %s", pl.Role)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		_, err = r.hostProbe(readyCtx, r.Plan, pl, r.expandProbe(probe, pl), false)
		if err == nil {
			break
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("credential readiness for %s: %w", pl.Instance, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
	if pl.Role != deploy.Database {
		script := "systemctl is-active --quiet " + shell(unit(r.Plan, pl)) + "\n" + credentialIdentityScript(r.Plan, pl) + credentialEnvironmentCheck() + "\n"
		if len(r.Plan.Release.Programs[pl.Role].Listeners) > 0 {
			script = shell(deploy.ContinuityBinary(r.Plan, pl)) + " verify " + shell(deploy.ContinuityDir(r.Plan, pl)) + " " + shell(cfgDir(r.Plan, pl)+"/launch.json") + "\n"
		}
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return fmt.Errorf("running credentials for %s differ: %w", pl.Instance, err)
		}
	} else if status[0] == "reloaded" {
		service, directory := shell(unit(r.Plan, pl)), cfgDir(r.Plan, pl)
		// Bind the receipt to the invocation inspected before reloading. If the
		// process exited meanwhile, leave activation pending for the next attempt.
		script := "systemctl is-active --quiet " + service + "\n"
		script += "invocation=$(systemctl show --value --property=InvocationID " + service + ")\ntest -n \"$invocation\"\n"
		script += credentialIdentityScript(r.Plan, pl)
		script += "test \"$invocation/$credential_digest\" = " + shell(status[1]) + "\n"
		script += remoteFile(directory+"/credentials-loaded", []byte(status[1]+"\n"))
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return fmt.Errorf("record credential reload for %s: %w", pl.Instance, err)
		}
	}
	if pl.Role == deploy.Envoy {
		return r.verifyEnvoyIdentity(ctx, pl)
	}
	return nil
}

func (r *Runner) prepareCredentialRestart(ctx context.Context, pl deploy.Placement) (func(context.Context) error, error) {
	switch pl.Role {
	case deploy.ControlPlane, deploy.Console, deploy.Envoy, deploy.Registry:
		probe, ok := r.Config.Probes[pl.Role]
		if !ok {
			return nil, fmt.Errorf("missing readiness probe for %s", pl.Role)
		}
		for _, peer := range r.Plan.Placements {
			if peer.Role != pl.Role || peer.Instance == pl.Instance {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := r.hostProbe(probeCtx, r.Plan, peer, r.expandProbe(probe, peer), false)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("%w: %s waits for ready peer %s: %v", errActivationPending, pl.Instance, peer.Instance, err)
			}
		}
	case deploy.Builder:
		return r.drainBuilderForRestart(ctx, pl, 2*time.Minute)
	}
	return func(context.Context) error { return nil }, nil
}

func (r *Runner) drainBuilderForRestart(ctx context.Context, pl deploy.Placement, wait time.Duration) (func(context.Context) error, error) {
	db, err := r.db(ctx, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	resume := func(ctx context.Context) error {
		db, err := r.db(ctx, false)
		if err != nil {
			return fmt.Errorf("restore builder %s drain intent: %w", pl.Instance, err)
		}
		defer db.Close()
		return resumeBuilder(ctx, db, credentialDrainKey, pl.Instance)
	}
	if err := drainBuilder(ctx, db, credentialDrainKey, pl.Instance); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		active, err := activeBuilds(waitCtx, db, pl.Instance)
		if err == nil && active == 0 {
			return resume, nil
		}
		if err == nil {
			err = sleepContext(waitCtx, 2*time.Second)
		}
		if err != nil {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			return nil, errors.Join(fmt.Errorf("%w: builder %s keeps its current credentials while builds finish: %v", errActivationPending, pl.Instance, err), resume(resumeCtx))
		}
	}
}

func (r *Runner) verifyEnvoyIdentity(ctx context.Context, pl deploy.Placement) error {
	script := "systemctl is-active --quiet " + shell(unit(r.Plan, pl)) + "\n" + shell(operationsBinary(r.Plan)) + " envoy-identity " + shell("http://"+envoyAdminAddress) + " " + shell(cfgDir(r.Plan, pl)+"/ingress/current/client.crt") + "\n"
	loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		_, err := r.remote(loadCtx, r.Plan, pl, script)
		if err == nil {
			return nil
		}
		if sleepContext(loadCtx, time.Second) != nil {
			return fmt.Errorf("running Envoy %s has not loaded its installed identity: %w", pl.Instance, err)
		}
	}
}
