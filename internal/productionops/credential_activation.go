package productionops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

const credentialDigestVariable = "PLATFORM_CREDENTIALS_DIGEST"

func credentialRuntimeEnvironment(files map[string][]byte, env map[string]string) []byte {
	if env == nil {
		env = map[string]string{}
	}
	delete(env, credentialDigestVariable)
	env[credentialDigestVariable] = deploy.Digest([]any{files, environment(env)})
	return environment(env)
}

func credentialIdentityScript(p deploy.Plan, pl deploy.Placement) string {
	return "credential_digest=$(sed -n 's/^" + credentialDigestVariable + "=\"\\([a-f0-9]*\\)\"$/\\1/p' " + shell(cfgDir(p, pl)+"/runtime.env") + ")\ntest \"${#credential_digest}\" = 64\n" +
		"pid=$(systemctl show --value --property=MainPID " + shell(unit(p, pl)) + ")\ntest \"$pid\" -gt 1\n"
}

func credentialEnvironmentCheck() string {
	return "tr '\\000' '\\n' < /proc/\"$pid\"/environ | grep -Fxq -- \"" + credentialDigestVariable + "=$credential_digest\""
}

// The environment fingerprints the complete installed credential contents.
// Rewrites of identical bytes do not activate anything; a failed restart remains
// detectable from the running process, even after another completer takes over.
func credentialActivationScript(p deploy.Plan, pl deploy.Placement) string {
	service, directory := shell(unit(p, pl)), cfgDir(p, pl)
	script := "if ! systemctl is-active --quiet " + service + "; then printf 'inactive\\n'; exit 0; fi\n"
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
		script += "systemctl restart " + service + "\nprintf 'restarted\\n'\n"
	}
	return script
}

func (r *Runner) activateCredentials(ctx context.Context, pl deploy.Placement) error {
	output, err := r.remote(ctx, r.Plan, pl, credentialActivationScript(r.Plan, pl))
	if err != nil {
		return fmt.Errorf("activate credentials for %s: %w", pl.Instance, err)
	}
	status := strings.Fields(string(output))
	if len(status) == 1 && status[0] == "inactive" {
		return nil // Maintenance never resurrects a stopped or retired unit.
	}
	if !(len(status) == 1 && (status[0] == "current" || status[0] == "restarted") || len(status) == 2 && status[0] == "reloaded" && pl.Role == deploy.Database) {
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
	return nil
}
