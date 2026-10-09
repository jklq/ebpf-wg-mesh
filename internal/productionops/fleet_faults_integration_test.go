//go:build integration && linux

package productionops

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/health"
)

func (f *nativeFleet) assertPause(p deploy.Plan, paused bool) {
	f.t.Helper()
	host := p.AdministrationHost
	address, _ := p.Installation.Host(host)
	query := `SELECT CASE WHEN a.paused THEN 'paused' ELSE 'active' END AS authority_state,CASE WHEN s.paused THEN 'paused' ELSE 'active' END AS scheduler_state FROM recovery_runtime_authority a CROSS JOIN build_scheduler_control s WHERE a.singleton=TRUE AND s.id=TRUE`
	b := f.docker("exec", f.hosts[host], f.selection.Database.Binary, "sql", "--host="+address.Network.SocketHost()+":26257", "--certs-dir="+f.selection.Database.CertificateDirectory+"/"+p.Generation, "--database="+f.selection.Database.Name, "--format=tsv", "--execute="+query)
	state := "active"
	if paused {
		state = "paused"
	}
	want := "\n" + state + "\t" + state + "\n"
	if !strings.Contains(string(b), want) {
		f.t.Fatal("native SQL automation pause differs", string(b))
	}
	for _, pl := range p.Placements {
		if !mutable(pl.Role) {
			continue
		}
		cfg := cfgDir(p, pl)
		// Native restart returns before a new listener necessarily binds. Wait
		// for the actual admitted process without weakening the pause assertion.
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		var report health.Report
		var lastErr error
		for {
			result, err := exec.CommandContext(ctx, "docker", "exec", f.hosts[pl.Host], operationsBinary(p), "probe", cfg+"/probe.json").CombinedOutput()
			if err == nil {
				err = json.Unmarshal(result, &report)
			}
			admitted := err == nil && report.Authority != nil && report.Authority.Paused == paused && report.Authority.Generation == p.Generation
			if p.Recovery && !paused && pl.Role == deploy.Agent {
				admitted = admitted && report.Status == health.StatusReady && report.Checkpoint != nil && report.Checkpoint.Complete && report.Checkpoint.Generation == p.Generation
			}
			if admitted {
				break
			}
			lastErr = err
			if ctx.Err() != nil {
				cancel()
				f.t.Fatal("actual running admission did not converge", pl.Instance, lastErr, report.Authority)
			}
			time.Sleep(250 * time.Millisecond)
		}
		cancel()
	}
}

// A failure after executing a real effect models loss of the installer before
// it records completion. All observation and retries still use the real driver.
type nativeInterruption struct {
	deploy.Driver
	hook        string
	interrupted bool
}

func (d *nativeInterruption) Execute(ctx context.Context, p deploy.Plan, s deploy.State, op deploy.Operation) (deploy.Binding, error) {
	b, err := d.Driver.Execute(ctx, p, s, op)
	if err == nil && !d.interrupted && op.Hook == d.hook {
		d.interrupted = true
		return b, context.Canceled
	}
	return b, err
}

func (f *nativeFleet) failedUpgrade(t *testing.T) {
	t.Helper()
	// Establish that the exact native gate succeeds before introducing the
	// outage, so another component failure cannot stand in for this fault.
	stateBefore, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var verify deploy.Operation
	for _, op := range f.plan.Operations {
		if op.Hook == "production-verify" {
			verify = op
		}
	}
	if done, _, err := f.driver.Observe(f.ctx, f.plan, stateBefore, verify); err != nil || !done {
		t.Fatal("native pre-upgrade production verification", err)
	}
	// Move a core replica during this upgrade. A failed activation must pause
	// the actual replacement and must not restart the retired prior replica.
	core := f.installation.Components[deploy.ControlPlane]
	core.Hosts = []string{"a", "c"}
	f.installation.Components[deploy.ControlPlane] = core
	f.release.ID = "r44"
	f.installation.Release = f.release.ID
	// Select the actual packaged native helpers staged for the applied release.
	base := "/opt/ebpf-wg-mesh/" + f.id + "/r44/tools/"
	f.selection.Database.Binary = base + "cockroachdb"
	f.selection.Console.AdminBinary = base + "console-admin"
	f.save(f.installation.OperationsConfig, f.selection)
	var rec map[string]any
	if err := privateJSON(f.selection.RecoveryConfig, &rec); err != nil {
		t.Fatal(err)
	}
	rec["release"] = "r44"
	rec["images"].(map[string]any)["binary"] = base + "skopeo"
	f.save(f.selection.RecoveryConfig, rec)
	p := f.build(false)
	f.mu.Lock()
	f.blocked = true
	f.mu.Unlock()
	engine := deploy.Engine{Store: f.store, Driver: f.driver}
	// Startup may require retrying native convergence. The injected HTTPS outage
	// must eventually fail the actual production verification hook.
	reached := false
	lastCompleted, stalled := -1, 0
	for attempts := 0; attempts < 25; attempts++ {
		err := engine.Apply(f.ctx, p)
		if err == nil {
			t.Fatal("upgrade resumed while its public endpoint was unavailable")
		}
		state, _ := f.store.Read()
		completed := len(state.Progress.Completed)
		t.Logf("native failed-upgrade attempt %d, completed=%d: %v", attempts+1, completed, err)
		if completed == lastCompleted {
			stalled++
		} else {
			stalled = 0
			lastCompleted = completed
		}
		for _, op := range p.Operations {
			if op.Hook == "production-verify" && state.Progress.Started[op.ID] {
				reached = true
			}
		}
		if reached || stalled >= 2 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !reached {
		f.diagnose(p)
		t.Fatal("upgrade did not reach native production verification")
	}
	gate := exec.CommandContext(f.ctx, "docker", "exec", "-e", "PLATFORM_PLAN=/etc/ebpf-wg-mesh/"+p.Installation.ID+"/plan.json", "-e", "PLATFORM_OPERATIONS_CONFIG="+p.Installation.OperationsConfig, f.hosts[p.AdministrationHost], operationsBinary(p), "verify", "production-verify")
	if body, err := gate.CombinedOutput(); err == nil || !strings.Contains(string(body), "service probe returned HTTP 503; expected 200") {
		f.diagnose(p)
		t.Fatalf("native production gate did not fail on the injected endpoint outage: %v: %s", err, body)
	}
	f.assertPause(p, true)
	state, _ := f.store.Read()
	for _, op := range p.Operations {
		if op.Hook == "resume" && state.Progress.Started[op.ID] {
			t.Fatal("resume executed after failed native production verification")
		}
	}
	if state.LastBackup.Backup == "" {
		t.Fatal("failed upgrade discarded its verified pre-upgrade recovery point")
	}
	// Host b ceased to be a completer when its core replica moved. Its old
	// renewal service must not later restart processes using the prior plan.
	for _, suffix := range []string{"-recovery.timer", "-recovery.service", "-recovery-credentials.timer", "-recovery-credentials.service"} {
		active := f.docker("exec", f.hosts["b"], "systemctl", "show", "--value", "-p", "ActiveState", "platform-"+f.id+suffix)
		state := strings.TrimSpace(string(active))
		if state != "inactive" && state != "failed" {
			t.Fatal("prior maintenance remains active after relocation", suffix, string(active))
		}
		if strings.HasSuffix(suffix, ".service") {
			pid := f.docker("exec", f.hosts["b"], "systemctl", "show", "--value", "-p", "MainPID", "platform-"+f.id+suffix)
			if strings.TrimSpace(string(pid)) != "0" {
				t.Fatal("prior maintenance retained a process after relocation", suffix, string(pid))
			}
		}
	}
	for _, suffix := range []string{"-recovery.timer", "-recovery-credentials.timer"} {
		disabled := f.docker("exec", f.hosts["b"], "sh", "-c", "systemctl is-enabled "+shell("platform-"+f.id+suffix)+" || true")
		if strings.TrimSpace(string(disabled)) != "disabled" {
			t.Fatal("prior maintenance could return after a reboot", suffix, string(disabled))
		}
	}
	f.mu.Lock()
	f.blocked = false
	f.mu.Unlock()
	interrupted := &nativeInterruption{Driver: f.driver, hook: "resume"}
	engine.Driver = interrupted
	if err := engine.Apply(f.ctx, p); err == nil {
		t.Fatal("post-resume interruption was not injected")
	}
	if !interrupted.interrupted {
		f.diagnose(p)
		t.Fatal("retry did not reach resume")
	}
	// The engine must undo a partial native resume before returning, then verify
	// again on retry. Inspect the actual process admission and SQL state.
	f.assertPause(p, true)
	for _, old := range stateBefore.Placements {
		if old.Role == deploy.ControlPlane && old.Host == "b" {
			active := f.docker("exec", f.hosts[old.Host], "systemctl", "show", "--value", "-p", "ActiveState", unit(p, old))
			if strings.TrimSpace(string(active)) != "inactive" {
				t.Fatal("interrupted resume restarted the retired core replica", string(active))
			}
		}
	}
	f.apply(p)
	f.assertPause(p, false)
	for _, suffix := range []string{"-recovery.timer", "-recovery-credentials.timer"} {
		active := f.docker("exec", f.hosts["b"], "systemctl", "show", "--value", "-p", "ActiveState", "platform-"+f.id+suffix)
		if strings.TrimSpace(string(active)) != "inactive" {
			t.Fatal("resume restarted prior maintenance on its retired completer", suffix, string(active))
		}
	}
	// Run the independently configured replacement completer on c. It regenerates
	// its own protected dependency closure; schedule verification must continue
	// inspecting fixed inputs and actual timers after that real service effect.
	f.completeService("c")
	state, err = f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range p.Operations {
		if op.Hook == "backup-schedule" {
			if done, _, err := f.driver.Observe(f.ctx, p, state, op); err != nil || !done {
				t.Fatal("resume retry lost the actual replacement backup schedules", err)
			}
		}
	}
	// Unit status alone cannot establish HTTP or SQL availability.
	for _, endpoint := range p.Installation.Endpoints {
		if _, err := probeHTTP(f.ctx, f.selection.EndpointProbes[endpoint.Name], false); err != nil {
			t.Fatal("upgraded public endpoint", endpoint.Name, err)
		}
	}
	// The relocation deliberately limited core placement to a/c. Apply a new
	// policy allowing all three suitable hosts before testing automatic failover;
	// the controller must never silently override an applied host constraint.
	core = f.installation.Components[deploy.ControlPlane]
	core.Hosts = []string{"a", "b", "c"}
	f.installation.Components[deploy.ControlPlane] = core
	f.apply(f.build(false))
}

func (f *nativeFleet) hostFailure(t *testing.T) {
	t.Helper()
	state, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := f.driver.Inventory(f.ctx, f.installation, f.release, state)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := deploy.BuildPlan(f.installation, f.release, state, inv, true, time.Now())
	if err != nil || !baseline.Availability.OneHostFailure {
		t.Fatal("actual native baseline lacks verified single-host redundancy", err, baseline.Availability)
	}
	f.docker("stop", "--time", "1", f.hosts["a"])
	lostAt := time.Now()
	// The public gateway selects a real listening backend before the request;
	// console and core clients must also find the surviving native database.
	for _, endpoint := range f.installation.Endpoints {
		var err error
		for attempts := 0; attempts < 20; attempts++ {
			if _, err = probeHTTP(f.ctx, f.selection.EndpointProbes[endpoint.Name], false); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			t.Fatal("public native path did not survive host loss", endpoint.Name, err)
		}
	}
	t.Log("measured public-path failover", time.Since(lostAt))
	p := f.build(true)
	if len(p.Purchases) != 0 || len(p.DatabaseChanges) != 0 {
		t.Fatal("automatic failover changed infrastructure or database membership")
	}
	for _, op := range p.Operations {
		if op.Host == "a" && op.Kind != "retain" {
			t.Fatal("automatic reconciliation mutated the unreachable host", op.Kind)
		}
	}
	f.apply(p)
	state, err = f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	inv, err = f.driver.Inventory(f.ctx, f.installation, f.release, state)
	if err != nil || len(inv.Database.Live) != 2 || len(inv.Database.Members) != 3 {
		t.Fatal("native failover did not preserve database membership and a live quorum", err, inv.Database)
	}
	for _, endpoint := range p.Installation.Endpoints {
		if _, err := probeHTTP(f.ctx, f.selection.EndpointProbes[endpoint.Name], false); err != nil {
			t.Fatal("reconciled public path", endpoint.Name, err)
		}
	}
}
