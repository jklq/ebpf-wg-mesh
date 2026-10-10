package productionops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

// Model systemd actions while inspecting real process environments, executing
// the actual generated shell, and making real HTTP readiness requests.
type credentialRemote struct {
	isolatedRemote
	binary       string
	controller   string
	probePath    string
	interruption string
}

func (r *credentialRemote) Run(ctx context.Context, i deploy.Installation, h deploy.Host, script string) ([]byte, error) {
	probe := strings.LastIndex(script, shell(r.binary)+" probe ")
	if probe >= 0 {
		if _, err := r.Run(ctx, i, h, script[:probe]); err != nil {
			return nil, err
		}
		var p Probe
		path := r.path(h, r.probePath)
		if err := privateJSON(path, &p); err != nil {
			return nil, err
		}
		return probeHTTP(ctx, p, false)
	}
	b, err := r.isolatedRemote.Run(ctx, i, h, strings.ReplaceAll(script, "systemctl", shell(r.controller)))
	if err == nil && r.interruption != "" && strings.Contains(script, r.interruption) {
		r.interruption = ""
		return nil, context.Canceled
	}
	return b, err
}

type credentialFixture struct {
	runner *Runner
	remote *credentialRemote
	pl     deploy.Placement
	root   string
}

func newCredentialFixture(t *testing.T, role deploy.Role) credentialFixture {
	t.Helper()
	r := testRunner(t)
	pl := deploy.Placement{Role: role, Host: "a", Instance: "a"}
	r.Plan.Placements = []deploy.Placement{pl}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(filepath.Join(r.Config.StateDirectory, "not-ready")); err == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	r.Config.Probes[role] = Probe{URL: server.URL, Status: 200}
	remote := &credentialRemote{isolatedRemote: r.Remote.(isolatedRemote), binary: operationsBinary(r.Plan), controller: filepath.Join(r.Config.StateDirectory, "systemctl"), probePath: cfgDir(r.Plan, pl) + "/inspection-probe.json"}
	r.Remote = remote
	root := r.Config.StateDirectory
	controller := `#!/bin/sh
set -eu
root=` + shell(root) + `
case "$1" in
is-active) test "$(cat "$root/state")" = active;;
show)
 case "$3" in
 --property=MainPID) cat "$root/pid";;
 --property=InvocationID) cat "$root/invocation";;
 *) exit 1;;
 esac;;
restart|kill)
 printf '%s\n' "$1" >> "$root/actions"
 test ! -f "$root/fail-action"
 if test "$1" = restart; then cp "$root/replacement-pid" "$root/pid"; fi;;
*) exit 1;;
esac
`
	if err := os.WriteFile(remote.controller, []byte(controller), 0700); err != nil {
		t.Fatal(err)
	}
	f := credentialFixture{runner: r, remote: remote, pl: pl, root: root}
	f.write(t, "state", "active")
	f.write(t, "invocation", "original-invocation")
	return f
}

func (f credentialFixture) write(t *testing.T, name, value string) {
	t.Helper()
	if err := writePrivate(filepath.Join(f.root, name), []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func (f credentialFixture) install(t *testing.T, key string) string {
	t.Helper()
	files := map[string][]byte{"client.key": []byte(key)}
	if _, err := f.runner.remote(context.Background(), f.runner.Plan, f.pl, remoteFile(cfgDir(f.runner.Plan, f.pl)+"/runtime.env", credentialRuntimeEnvironment(files, nil))); err != nil {
		t.Fatal(err)
	}
	env := credentialRuntimeEnvironment(files, nil)
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(string(env)), credentialDigestVariable+"="), "\"")
}

func credentialProcess(t *testing.T, digest string) string {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	cmd.Env = append(os.Environ(), credentialDigestVariable+"="+digest)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return fmt.Sprint(cmd.Process.Pid)
}

func (f credentialFixture) actions(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "actions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func TestCredentialActivationLeavesIdenticalRewritesRunning(t *testing.T) {
	for _, role := range []deploy.Role{deploy.Builder, deploy.Database} {
		t.Run(string(role), func(t *testing.T) {
			f := newCredentialFixture(t, role)
			digest := f.install(t, "original private material")
			f.write(t, "pid", credentialProcess(t, digest))
			// Issuance rewrites files, but byte equality must keep the process alive.
			f.install(t, "original private material")
			if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
				t.Fatal(err)
			}
			if actions := f.actions(t); len(actions) != 0 {
				t.Fatal("unchanged credentials interrupted work", actions)
			}
		})
	}
}

func TestCredentialActivationRetriesChangedMaterial(t *testing.T) {
	for _, interruption := range []string{"restart-failure", "after-restart", "readiness-failure"} {
		t.Run(interruption, func(t *testing.T) {
			f := newCredentialFixture(t, deploy.Console)
			f.write(t, "pid", credentialProcess(t, f.install(t, "old private material")))
			newDigest := f.install(t, "renewed private material")
			f.write(t, "replacement-pid", credentialProcess(t, newDigest))
			switch interruption {
			case "restart-failure":
				f.write(t, "fail-action", "")
			case "after-restart":
				f.remote.interruption = "systemctl restart"
			case "readiness-failure":
				f.write(t, "not-ready", "")
			}
			timeout := 5 * time.Second
			if interruption == "readiness-failure" {
				timeout = 500 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if f.runner.activateCredentials(ctx, f.pl) == nil {
				t.Fatal("interrupted activation reported success")
			}
			for _, name := range []string{"fail-action", "not-ready"} {
				if err := os.Remove(filepath.Join(f.root, name)); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			// Same installed bytes on retry: a before/after comparison alone would
			// incorrectly skip activation if the first restart failed.
			f.install(t, "renewed private material")
			if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
				t.Fatal(err)
			}
			if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
				t.Fatal(err)
			}
			want := 1
			if interruption == "restart-failure" {
				want = 2 // One failed action and one successful retry.
			}
			if actions := f.actions(t); len(actions) != want {
				t.Fatal("activation repeated after the new credentials were loaded", actions)
			}
		})
	}
}

func TestDatabaseCredentialReloadReceiptSurvivesRetryAndFencesInvocation(t *testing.T) {
	f := newCredentialFixture(t, deploy.Database)
	f.write(t, "pid", credentialProcess(t, f.install(t, "old node certificate")))
	f.install(t, "renewed node certificate")
	f.remote.interruption = "test \"$invocation/$credential_digest\" ="
	if f.runner.activateCredentials(context.Background(), f.pl) == nil {
		t.Fatal("lost reload acknowledgement reported success")
	}
	if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
		t.Fatal(err)
	}
	if actions := f.actions(t); len(actions) != 1 || actions[0] != "kill" {
		t.Fatal("acknowledged reload was repeated or restarted the database", actions)
	}
	f.write(t, "invocation", "replacement-invocation")
	if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
		t.Fatal(err)
	}
	if actions := f.actions(t); len(actions) != 2 {
		t.Fatal("old process receipt admitted replacement credentials", actions)
	}
}

func TestDatabaseCredentialRollbackReloadsDespiteInitialEnvironment(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			f := newCredentialFixture(t, deploy.Database)
			f.write(t, "pid", credentialProcess(t, f.install(t, "original node certificate")))
			f.install(t, "renewed node certificate")
			if interrupted {
				f.remote.interruption = "systemctl kill"
			}
			err := f.runner.activateCredentials(context.Background(), f.pl)
			if (err != nil) != interrupted {
				t.Fatal("unexpected reload result", err)
			}
			f.install(t, "original node certificate")
			if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
				t.Fatal(err)
			}
			if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
				t.Fatal(err)
			}
			if actions := f.actions(t); len(actions) != 2 {
				t.Fatal("rollback trusted the database's initial environment", actions)
			}
		})
	}
}

func TestCredentialActivationPreservesStoppedUnits(t *testing.T) {
	for _, role := range []deploy.Role{deploy.Agent, deploy.Database} {
		f := newCredentialFixture(t, role)
		f.write(t, "state", "inactive")
		if err := f.runner.activateCredentials(context.Background(), f.pl); err != nil {
			t.Fatal(err)
		}
		if actions := f.actions(t); len(actions) != 0 {
			t.Fatal("maintenance resurrected a stopped unit", actions)
		}
	}
}
