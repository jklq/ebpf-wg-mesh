//go:build integration && linux

package productionops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"github.com/google/uuid"
)

type systemdMaintenanceRemote struct{ container string }

func (r systemdMaintenanceRemote) Run(ctx context.Context, _ deploy.Installation, _ deploy.Host, script string) ([]byte, error) {
	b, err := exec.CommandContext(ctx, "docker", "exec", r.container, "sh", "-eu", "-c", script).CombinedOutput()
	if err != nil {
		return b, fmt.Errorf("native systemd: %w: %s", err, b)
	}
	return b, nil
}
func (systemdMaintenanceRemote) Upload(context.Context, deploy.Installation, deploy.Host, string, string, string) error {
	return fmt.Errorf("maintenance must not upload executables")
}

func TestNativePriorMaintenanceStopsOldJobsAndPreservesReplacementOnRetry(t *testing.T) {
	if os.Getenv("PRODUCTION_FLEET_INTEGRATION") != "1" {
		t.Skip("set PRODUCTION_FLEET_INTEGRATION=1 and provide the native systemd host image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := "maintenance" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	image := os.Getenv("PRODUCTION_HOST_IMAGE")
	if image == "" {
		image = "platform-operations-fixture/host:r43"
	}
	if b, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", id, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", image).CombinedOutput(); err != nil {
		t.Fatalf("native host: %v: %s", err, b)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "--force", id).Run() })
	host := deploy.Host{ID: "a", Binding: deploy.Binding{Provider: "linux", ServerID: id}, Trusted: true, Reliability: "reliable"}
	i := deploy.Installation{ID: id, Hosts: []deploy.Host{host}, Providers: map[string]deploy.Provider{"linux": {Kind: "linux"}}}
	r := Runner{Plan: deploy.Plan{ID: "replacement", Installation: i, Release: deploy.Release{ID: "r44"}, AdministrationHost: "a", Placements: []deploy.Placement{{Role: deploy.ControlPlane, Host: "a"}}, Previous: &deploy.AppliedDeployment{Installation: i}}, Config: Config{StateDirectory: "/var/lib/ebpf-wg-mesh/" + id}, Remote: systemdMaintenanceRemote{container: id}}
	r.defaults()
	run := func(script string) []byte {
		t.Helper()
		b, err := r.remote(ctx, r.Plan, deploy.Placement{Host: "a"}, script)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for attempts := 0; ; attempts++ {
		if _, err := r.remote(ctx, r.Plan, deploy.Placement{Host: "a"}, "systemctl list-jobs --no-pager\n"); err == nil {
			break
		} else if attempts == 50 {
			t.Fatal("native systemd did not start", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	name := "platform-" + id + "-recovery"
	for _, service := range []string{name, name + "-credentials"} {
		job := "[Service]\nType=simple\nExecStart=/usr/bin/sleep 600\n"
		timer := "[Timer]\nOnUnitActiveSec=1h\n[Install]\nWantedBy=timers.target\n"
		run(remoteFile("/etc/systemd/system/"+service+".service", []byte(job)) + remoteFile("/etc/systemd/system/"+service+".timer", []byte(timer)) + "systemctl daemon-reload\nsystemctl enable --now " + shell(service+".timer") + "\nsystemctl start " + shell(service+".service") + "\nsystemctl is-active --quiet " + shell(service+".service") + "\n")
	}
	if r.priorMaintenance(ctx, false) == nil {
		t.Fatal("running old jobs passed verification")
	}
	if err := r.stopPriorMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.priorMaintenance(ctx, false); err != nil {
		t.Fatal("old native jobs were not persistently stopped", err)
	}
	for file, content := range r.completionUnits() {
		run(remoteFile("/etc/systemd/system/"+file, []byte(content)))
	}
	run("systemctl daemon-reload\nsystemctl enable --now " + shell(name+".timer") + " " + shell(name+"-credentials.timer") + "\n")
	for attempts := 0; attempts < 2; attempts++ {
		if err := r.stopPriorMaintenance(ctx); err != nil {
			t.Fatal("partial-resume pause", err)
		}
		if err := r.priorMaintenance(ctx, false); err != nil {
			t.Fatal("replacement admission", err)
		}
		run("systemctl is-active --quiet " + shell(name+".timer") + "\nsystemctl is-active --quiet " + shell(name+"-credentials.timer") + "\n")
	}
	// Unknown launch inputs cannot impersonate the admitted replacement.
	run("printf '\n# altered launch inputs\n' >> " + shell("/etc/systemd/system/"+name+".service") + "\n")
	if r.priorMaintenance(ctx, false) == nil {
		t.Fatal("altered active maintenance passed verification")
	}
	if err := r.stopPriorMaintenance(ctx); err != nil {
		t.Fatal("altered maintenance could not be stopped", err)
	}
	t.Run("masked-starting-process-is-not-fenced", func(t *testing.T) {
		pl := deploy.Placement{Role: deploy.Database, Host: "a", Instance: "starting"}
		r.Plan.Placements = []deploy.Placement{pl}
		name := unit(r.Plan, pl)
		path := "/etc/systemd/system/" + name
		job := "[Service]\nType=notify\nExecStart=/usr/bin/sleep 600\nTimeoutStartSec=10min\n"
		run(remoteFile(path, []byte(job)) + "systemctl daemon-reload\nsystemctl start --no-block " + shell(name) + "\n")
		for attempts := 0; ; attempts++ {
			state := run("systemctl show --value -p ActiveState -p MainPID " + shell(name) + "\n")
			if strings.Contains(string(state), "activating") && !strings.Contains("\n"+strings.TrimSpace(string(state))+"\n", "\n0\n") {
				break
			}
			if attempts == 50 {
				t.Fatal("native process did not start", string(state))
			}
			time.Sleep(20 * time.Millisecond)
		}
		// A persistent mask does not stop a launch already in progress. The
		// real process is still alive even though is-active returns false.
		run("mv " + shell(path) + " " + shell(path+".original") + "\nln -s /dev/null " + shell(path) + "\nsystemctl daemon-reload\ntest \"$(systemctl show --value -p ActiveState " + shell(name) + ")\" = activating\n! systemctl is-active --quiet " + shell(name) + "\n")
		if r.verifyHostFenced(ctx, r.Plan, "a") == nil {
			t.Fatal("masked starting process was accepted as fenced")
		}
		for attempts := 0; attempts < 2; attempts++ {
			run(maskScript(r.Plan, pl))
			if err := r.verifyHostFenced(ctx, r.Plan, "a"); err != nil {
				t.Fatal("native masked process did not stop safely on retry", err)
			}
		}
	})
}
