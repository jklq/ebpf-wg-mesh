//go:build integration && linux

package productionops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

// The selected out-of-band fence controls actual fixture hosts and reads their
// power state. A failed request or an unknown host cannot claim
// a fence, and a reset response alone never establishes that power is off.
func (f *nativeFleet) installFences() {
	f.write(f.root+"/fence-credentials.json", []byte(`{"username":"fixture-admin","password":"isolated-fixture-only"}`))
	server := f.tlsServer(f.serviceCA, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, password, ok := req.BasicAuth()
		if !ok || user != "fixture-admin" || password != "isolated-fixture-only" {
			w.WriteHeader(401)
			return
		}
		id := strings.TrimPrefix(req.URL.Path, "/redfish/v1/Systems/")
		reset := strings.HasSuffix(id, "/Actions/ComputerSystem.Reset")
		id = strings.TrimSuffix(id, "/Actions/ComputerSystem.Reset")
		f.mu.RLock()
		name := f.hosts[id]
		f.mu.RUnlock()
		if name == "" {
			w.WriteHeader(404)
			return
		}
		if req.Method == http.MethodPost && reset {
			var body struct{ ResetType string }
			if json.NewDecoder(req.Body).Decode(&body) != nil || body.ResetType != "ForceOff" {
				w.WriteHeader(400)
				return
			}
			if err := exec.CommandContext(f.ctx, "docker", "stop", "--time", "1", name).Run(); err != nil {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(204)
			return
		}
		if req.Method != http.MethodGet || reset {
			w.WriteHeader(405)
			return
		}
		b, err := exec.CommandContext(f.ctx, "docker", "inspect", "--format", "{{.State.Running}}", name).Output()
		if err != nil {
			w.WriteHeader(503)
			return
		}
		power := "On"
		if strings.TrimSpace(string(b)) == "false" {
			power = "Off"
		}
		json.NewEncoder(w).Encode(map[string]any{"PowerState": power, "Actions": map[string]any{"#ComputerSystem.Reset": map[string]string{"target": "/redfish/v1/Systems/" + id + "/Actions/ComputerSystem.Reset"}}})
	}))
	f.fenceURL = server.URL
	f.selection.Fences = map[string]RedfishFence{}
	for id := range f.hosts {
		f.selection.Fences[id] = RedfishFence{SystemURL: server.URL + "/redfish/v1/Systems/" + id, CredentialsFile: f.root + "/fence-credentials.json", CAFile: f.root + "/services-ca.crt"}
	}
}

func (f *nativeFleet) sql(p deploy.Plan, query string) []byte {
	h, ok := p.Installation.Host(p.AdministrationHost)
	if !ok {
		f.t.Fatal("no native administration host")
	}
	return f.docker("exec", f.hosts[h.ID], f.selection.Database.Binary, "sql", "--host="+net.JoinHostPort(h.Network.Address, "26257"), "--certs-dir="+f.selection.Database.CertificateDirectory+"/"+p.Generation, "--database="+f.selection.Database.Name, "--format=tsv", "--execute="+query)
}

func (f *nativeFleet) completePoint() deploy.Evidence {
	f.t.Helper()
	p := f.plan
	// Submit a real native backup, then explicitly run the independent OS service.
	// It uses the applied plan and actual SQL/artifact/key inspection, with no
	// platform session. There is no fabricated completion timestamp or receipt.
	f.sql(p, "BACKUP INTO 'external://native_recovery' WITH revision_history")
	f.completeService(p.AdministrationHost)
	cfg, service, err := recovery.Load(f.selection.RecoveryConfig)
	if err != nil {
		f.t.Fatal(err)
	}
	defer clear(service.RecoveryKey)
	versions, err := service.Storage.Versions(f.ctx, service.Prefix+"/points/"+f.id+"/")
	if err != nil {
		f.t.Fatal(err)
	}
	var selected deploy.Evidence
	for _, object := range versions {
		object, err = service.Storage.Inspect(f.ctx, object)
		if err != nil {
			f.t.Fatal(err)
		}
		point, err := service.ReadPoint(f.ctx, object)
		if err != nil {
			f.t.Fatal(err)
		}
		if point.Snapshot.Timestamp.After(selected.DataLossCutoff) {
			selected = deploy.Evidence{Backup: object.S3URL(cfg.Storage.Bucket), DataLossCutoff: point.Snapshot.Timestamp, Point: &point, Object: &object}
		}
	}
	if selected.Point == nil || !service.Verify(f.ctx, *selected.Point, true).Complete {
		f.t.Fatal("no actual complete native point")
	}
	// Inspect the actual encrypted dependency, including points created before
	// activation. Those points must start a new recovery without an old session.
	for _, dependency := range selected.Point.Dependencies {
		if dependency.Kind != "deployment-state" {
			continue
		}
		path := filepath.Join(f.ram, "protected-installer.json")
		if err := service.Materialize(f.ctx, dependency, path, true); err != nil {
			f.t.Fatal(err)
		}
		var snapshot deploy.State
		if err := privateJSON(path, &snapshot); err != nil {
			f.t.Fatal(err)
		}
		if snapshot.Progress != nil || snapshot.Recovery != nil {
			f.t.Fatal("protected point carries an unfinished installer session")
		}
	}
	return selected
}

func (f *nativeFleet) completeService(host string) {
	f.maintenanceService(host, "-recovery.service")
}

func (f *nativeFleet) maintenanceService(host, suffix string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Minute)
	defer cancel()
	name := "platform-" + f.id + suffix
	var last []byte
	for attempt := 0; attempt < 30; attempt++ {
		command := exec.CommandContext(ctx, "docker", "exec", f.hosts[host], "systemctl", "start", name)
		var err error
		last, err = command.CombinedOutput()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			break
		}
		// Another actual completer may still hold the native publishing lease.
		// A successful service is followed by complete point verification.
		time.Sleep(2 * time.Second)
	}
	journal, _ := exec.CommandContext(f.ctx, "docker", "exec", f.hosts[host], "journalctl", "--no-pager", "-u", name, "-n", "15").CombinedOutput()
	f.t.Fatalf("native independent maintenance did not finish: %s\n%s", last, journal)
}

func (f *nativeFleet) createQuarantinedContainer(p deploy.Plan, host string) string {
	f.t.Helper()
	var agent deploy.Placement
	for _, pl := range p.Placements {
		if pl.Role == deploy.Agent && pl.Host == host {
			agent = pl
		}
	}
	if agent.Instance == "" {
		f.t.Fatal("surviving host lacks native agent")
	}
	// Stop the old agent before creating post-cutoff state. Recovery restarts it
	// paused, inventories the actual container and requires report approval.
	f.docker("exec", f.hosts[host], "systemctl", "stop", unit(p, agent))
	layout := os.Getenv("PRODUCTION_TOOLCHAIN_OCI")
	if layout == "" {
		layout = "/dev/shm/platform-toolchain-oci"
	}
	archivePath := filepath.Join(f.ram, "quarantine-toolchain.tar")
	f.command(filepath.Join(f.releaseDir, "skopeo-amd64"), "--insecure-policy", "copy", "--all", "--preserve-digests", "oci:"+layout, "oci-archive:"+archivePath+":quarantine-toolchain")
	archive, err := os.Open(archivePath)
	if err != nil {
		f.t.Fatal(err)
	}
	defer archive.Close()
	importer := exec.CommandContext(f.ctx, "docker", "exec", "-i", f.hosts[host], "ctr", "--namespace", "default", "images", "import", "--snapshotter", "native", "-")
	importer.Stdin = archive
	if b, err := importer.CombinedOutput(); err != nil {
		f.t.Fatalf("native image import: %v: %s", err, b)
	}
	id := "post-cutoff-" + f.id
	args := []string{"exec", f.hosts[host], "ctr", "--namespace", "default", "containers", "create", "--snapshotter", "native"}
	for _, label := range []string{"platform.managed=true", "platform.allocation_id=" + id, "platform.allocation_created_at=" + time.Now().UTC().Format(time.RFC3339Nano), "platform.service_id=unknown-service", "platform.environment_id=unknown-environment", "platform.deployment_id=unknown-deployment", "mesh.environment_id=999999", "mesh.ipv4=10.200.250.2", "mesh.ipv6=fd00:200::ffff"} {
		args = append(args, "--label", label)
	}
	// ctr import retains the actual OCI manifest name. Inspect it, rather than
	// assigning a synthetic image digest or bypassing the native runtime.
	images := strings.Fields(string(f.docker("exec", f.hosts[host], "ctr", "--namespace", "default", "images", "list", "--quiet")))
	if len(images) == 0 {
		f.t.Fatal("native containerd did not import the toolchain")
	}
	args = append(args, images[0], "platform-"+id, "/bin/true")
	f.docker(args...)
	// This native process was also absent from the saved installer inventory.
	// Fencing must discover it on the actual host, preserve its definition and
	// prevent its independently queued timer from reviving the old authority.
	name := "platform-" + f.id + "-post-cutoff"
	service := "[Service]\nType=simple\nExecStart=/usr/bin/sleep 600\n"
	timer := "[Timer]\nOnUnitActiveSec=1h\n[Install]\nWantedBy=timers.target\n"
	script := remoteFile("/etc/systemd/system/"+name+".service", []byte(service)) + remoteFile("/etc/systemd/system/"+name+".timer", []byte(timer))
	script += "systemctl daemon-reload\nsystemctl enable --now " + shell(name+".timer") + "\nsystemctl start " + shell(name+".service") + "\nsystemctl is-active --quiet " + shell(name+".service") + "\n"
	f.docker("exec", f.hosts[host], "sh", "-eu", "-c", script)
	return id
}

func (f *nativeFleet) restoreFleet(t *testing.T, siteLoss bool) {
	t.Helper()
	workID := fmt.Sprintf("external-effect-%s-%t", f.id, siteLoss)
	sessionID := fmt.Sprintf("old-session-%s-%t", f.id, siteLoss)
	// Seed actual retained authority state before native SQL capture. The leased
	// external effect has not expired, so live workers cannot replay it.
	f.sql(f.plan, fmt.Sprintf(`INSERT INTO dashboard.users(id,email,created_at,updated_at) VALUES('recovery-user','operator@example.invalid',now(),now()) ON CONFLICT DO NOTHING;
INSERT INTO dashboard.refresh_sessions(id,user_id,created_at,expires_at) VALUES('%s','recovery-user',now(),now()+INTERVAL '1 hour');
INSERT INTO durable_work_items(id,kind,dedup_key,state,attempt_limit,owner_id,owner_epoch,lease_expires_at,available_at,payload,created_at,updated_at) VALUES('%s','effect','%s','leased',3,'lost-authority',9,now()+INTERVAL '1 hour',now(),'{"external":"preserve","request":"%s"}',now(),now())`, sessionID, workID, workID, workID))
	selected := f.completePoint()
	originalPlan := f.plan
	originalGeneration := originalPlan.Generation
	orphan := ""
	if !siteLoss {
		orphan = f.createQuarantinedContainer(originalPlan, "b")
	}
	declared := time.Now()
	t.Logf("measured metadata cutoff age at loss, siteLoss=%t: %s", siteLoss, declared.Sub(selected.DataLossCutoff))
	if declared.Sub(selected.DataLossCutoff) > recovery.Objective {
		t.Fatal("actual selected native point exceeds the metadata objective")
	}
	// Independent copies are kept outside the simulated operator disk. No
	// platform account, local keyring or functioning old core is needed.
	independent := filepath.Join(f.ram, "independent")
	if err := os.MkdirAll(independent, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, svc, err := recovery.Load(f.selection.RecoveryConfig)
	if err != nil {
		t.Fatal(err)
	}
	clear(svc.RecoveryKey)
	for name, path := range map[string]string{"recovery.key": cfg.RecoveryKeyFile, "writer.ini": cfg.Storage.CredentialsFile, "ca.crt": cfg.Storage.CAFile} {
		f.write(independent+"/"+name, mustRead(t, path))
	}
	cfg.RecoveryKeyFile = independent + "/recovery.key"
	cfg.Storage.CredentialsFile = independent + "/writer.ini"
	cfg.Storage.CAFile = independent + "/ca.crt"
	f.save(independent+"/recovery.json", cfg)
	operatorBinary := independent + "/operations"
	f.write(operatorBinary, mustRead(t, filepath.Join(f.releaseDir, "operations-amd64")))
	if err := os.Chmod(operatorBinary, 0700); err != nil {
		t.Fatal(err)
	}
	if siteLoss {
		for _, h := range originalPlan.Installation.Hosts {
			f.docker("stop", "--time", "1", f.hosts[h.ID])
		}
		// Lose the stopped site's private host disks as well as its processes.
		// Retain the powered-off provider bindings so fencing can independently
		// inspect actual power state while restoring onto entirely new hosts.
		f.docker("run", "--rm", "--mount", "type=bind,src="+f.ram+",dst=/owned", f.image, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/owned")
		for _, h := range originalPlan.Installation.Hosts {
			if err := os.RemoveAll(filepath.Join(f.ram, h.ID)); err != nil {
				t.Fatal("lose isolated host disk", err)
			}
		}
	}
	f.store.Close()
	if f.artifacts != nil {
		f.artifacts.Close()
		f.artifacts = nil
	}
	if f.publisher != nil {
		f.publisher.Process.Kill()
		f.publisher.Wait()
		f.publisher = nil
	}
	// Delete the private operator inputs/state. The remaining out-of-band key
	// and writer configuration are sufficient to recover exact pinned tools.
	oldRoot := f.root
	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Operator inputs must use the same durable path rules as an installation.
	// Native host data and services remain isolated in the fixture's RAM mounts.
	destination, err := os.MkdirTemp(home, ".platform-recovered-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(destination) })
	command := exec.CommandContext(f.ctx, operatorBinary, "recover-files", independent+"/recovery.json", selected.Backup, destination)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("packaged independent recovery: %v: %s", err, output)
	}
	var recovered recoveredFiles
	if err := json.Unmarshal(output, &recovered); err != nil {
		t.Fatal("packaged independent recovery response", err)
	}
	diagnostic := exec.CommandContext(f.ctx, recovered.Tools[originalPlan.Release.ID+"/tool/aws/amd64"], "--version")
	diagnostic.Env = append(os.Environ(), "PLATFORM_RECOVERY_WORKSPACE="+independent+"/native-bundles")
	if err := diagnostic.Run(); err != nil {
		t.Fatal("independently recovered AWS executable", err)
	}
	databaseTool := recovered.Tools[originalPlan.Release.ID+"/tool/cockroachdb/amd64"]
	if databaseTool == "" || databaseTool != recovered.Tools[originalPlan.Release.ID+"/program/cockroachdb/amd64"] {
		t.Fatal("independent identical database executables were not recovered with both release identities")
	}
	var target deploy.Installation
	if err = privateJSON(recovered.Installation, &target); err != nil {
		t.Fatal(err)
	}
	if err = privateJSON(recovered.Release, &f.release); err != nil {
		t.Fatal(err)
	}
	if err = privateJSON(recovered.OperationsConfig, &f.selection); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.root = destination
	f.mu.Unlock()
	// Mutable runtime state uses the host's durable native storage; independently
	// recovered operator files remain private in the isolated workspace.
	f.selection.StateDirectory = "/var/lib/ebpf-wg-mesh/" + f.id + "/operations"
	f.selection.Database.CertificateDirectory = "/var/lib/ebpf-wg-mesh/" + f.id + "/database-pki"
	ids := []string{"b", "c", "d"}
	if siteLoss {
		ids = []string{"e", "f", "g"}
	}
	oldHosts := append([]deploy.Host{}, target.Hosts...)
	target.Hosts = nil
	for _, id := range ids {
		var host deploy.Host
		for _, old := range oldHosts {
			if old.ID == id {
				host = old
			}
		}
		if host.ID == "" {
			host = oldHosts[0]
			host.ID = id
			host = f.prepareHost(host)
		}
		target.Hosts = append(target.Hosts, host)
	}
	target.ManagementHost = ids[0]
	// This applied recovery plan selects the replacement fleet, including the
	// core placement constraint used to exercise relocation during the upgrade.
	for role, component := range target.Components {
		if len(component.Hosts) > 0 {
			component.Hosts = append([]string{}, ids...)
			target.Components[role] = component
		}
	}
	for n := range target.Hosts {
		target.Hosts[n].Network.Peers = map[string]int{}
		for _, peer := range ids {
			if peer != target.Hosts[n].ID {
				target.Hosts[n].Network.Peers[peer] = 5
			}
		}
	}
	for name, storage := range target.Storage {
		storage.Hosts = append([]string{}, ids...)
		target.Storage[name] = storage
	}
	for n := range target.Endpoints {
		target.Endpoints[n].Hosts = append([]string{}, ids...)
	}
	target.Recovery.Hosts = nil
	spare := target.Hosts[0]
	spare.ID = "recovery"
	spare.Binding.ServerID = f.id + "-recovery"
	spare.FailureDomain = "fixture/recovery"
	target.Recovery.Hosts = []deploy.Host{spare}
	for _, host := range target.Hosts {
		if _, ok := f.selection.Fences[host.ID]; !ok {
			f.selection.Fences[host.ID] = RedfishFence{SystemURL: f.fenceURL + "/redfish/v1/Systems/" + host.ID, CredentialsFile: f.selection.Fences[oldHosts[0].ID].CredentialsFile, CAFile: f.selection.Fences[oldHosts[0].ID].CAFile}
		}
	}
	target.OperationsInputs = append(target.OperationsInputs, destination+"/known_hosts")
	// The first restore preserves existing b/c host keys. If no replacement was
	// needed, ensure the additional inventory file is still a private real file.
	if _, err := os.Stat(destination + "/known_hosts"); os.IsNotExist(err) {
		f.write(destination+"/known_hosts", nil)
	}
	f.save(target.OperationsConfig, f.selection)
	f.save(recovered.Installation, target)
	f.installation = target
	f.driver = deploy.NewSSHDriver()
	f.driver.RecoveryConfig = recovered.RecoveryConfig
	var restoreOutput bytes.Buffer
	err = deploy.Run(f.ctx, []string{"restore", "--state", recovered.State, "--key-file", recovered.StateKey, "--installation", recovered.Installation, "--bundle", recovered.Release, "--recovery-config", recovered.RecoveryConfig, "--backup", selected.Backup, "--data-loss-cutoff", selected.DataLossCutoff.Format(time.RFC3339Nano)}, &restoreOutput, f.driver)
	var errOpen error
	f.store, errOpen = deploy.OpenState(recovered.State, mustRead(t, recovered.StateKey))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	state, errRead := f.store.Read()
	if errRead != nil {
		t.Fatal(errRead)
	}
	if state.Progress == nil || state.Progress.Plan == nil {
		t.Fatal("restore did not record its concrete recovery plan", err, restoreOutput.String())
	}
	p := *state.Progress.Plan
	f.mu.Lock()
	f.plan = p
	f.mu.Unlock()
	engine := deploy.Engine{Store: f.store, Driver: f.driver}
	interrupted := &nativeInterruption{Driver: f.driver, hook: "recovery-reconcile"}
	// The surviving-fleet case also loses the installer after actual activation.
	// The engine must re-pause before re-verifying recovery admission on retry.
	activation := &nativeInterruption{Driver: interrupted, hook: "recovery-resume", interrupted: siteLoss}
	engine.Driver = activation
	lastCompleted, stalled := -1, 0
	approved := false
	for attempts := 0; attempts < 35; attempts++ {
		err = engine.Apply(f.ctx, p)
		state, errRead = f.store.Read()
		if errRead != nil {
			t.Fatal(errRead)
		}
		if errors.Is(err, deploy.ErrRecoveryApproval) {
			if state.Recovery == nil || state.Recovery.Report == nil || state.Recovery.Report.Blocked {
				t.Fatal("native recovery report blocks approval", state.Recovery)
			}
			if orphan != "" {
				found := false
				for _, difference := range state.Recovery.Report.Differences {
					if difference.Resource == orphan && difference.Kind == "quarantined-allocation" {
						found = true
					}
				}
				if !found {
					t.Fatal("actual post-cutoff container omitted from quarantine report")
				}
				reserved := false
				for _, reservation := range state.Recovery.Report.Reservations {
					if reservation.Identity == 999999 {
						reserved = true
					}
				}
				if !reserved {
					t.Fatal("post-cutoff network identity was not reserved additively")
				}
			}
			f.assertPause(p, true)
			if err = engine.ApproveRecovery(state.Recovery.Report.ApprovalDigest()); err != nil {
				t.Fatal(err)
			}
			t.Logf("native recovery report approved, siteLoss=%t", siteLoss)
			approved = true
			stalled = 0
			continue
		}
		if err == nil {
			break
		}
		completed := len(state.Progress.Completed)
		t.Logf("native recovery attempt %d completed=%d: %v", attempts+1, completed, err)
		if interrupted.interrupted {
			f.assertPause(p, true)
		}
		if completed == lastCompleted {
			stalled++
		} else {
			stalled = 0
			lastCompleted = completed
		}
		if stalled >= 3 {
			f.diagnose(p)
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	state, err = f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !approved || !interrupted.interrupted || !activation.interrupted || p.Generation == originalGeneration || state.Recovery == nil || !state.Recovery.Verified || state.Recovery.CompletedAt.IsZero() || state.Recovery.NewRecoveryPoint == nil {
		t.Fatal("native recovery did not complete its verified gates", state.Recovery)
	}
	f.assertPause(p, false)
	if !siteLoss {
		// Renewal runs independently of the installer and must preserve the
		// admitted, complete checkpoint and quarantined ownership after resume.
		f.maintenanceService(p.AdministrationHost, "-recovery-credentials.service")
		f.assertPause(p, false)
		t.Log("native post-restore credential renewal preserved admission")
	}
	if rows := f.sql(p, "SELECT count(*) FROM dashboard.refresh_sessions"); !strings.Contains(string(rows), "\n0\n") {
		t.Fatal("native restored console sessions survived authority replacement", string(rows))
	}
	// Attempt completion with the old ownership guard, then inspect the actual
	// unchanged row and its full quarantine payload through native SQL.
	rows := f.sql(p, fmt.Sprintf(`UPDATE durable_work_items SET state='succeeded' WHERE id='%s' AND state='leased' AND owner_id='lost-authority' AND owner_epoch=9;
SELECT state,owner_epoch,payload->>'external' FROM durable_work_items WHERE id='%s';
SELECT record->>'owner_id',record->>'owner_epoch',record->'payload'->>'request' FROM platform_recovery.public.work_quarantine WHERE generation='%s' AND kind='durable_work_items' AND id='%s'`, workID, workID, p.Generation, workID))
	if !strings.Contains(string(rows), "dead\t10\tpreserve") || !strings.Contains(string(rows), "lost-authority\t9\t"+workID) {
		t.Fatal("native work recovery lost payload or admitted stale ownership", string(rows))
	}
	if orphan != "" {
		f.docker("exec", f.hosts["b"], "ctr", "--namespace", "default", "containers", "info", "platform-"+orphan)
		name := "platform-" + f.id + "-post-cutoff"
		for _, suffix := range []string{".service", ".timer"} {
			script := "test \"$(systemctl is-enabled " + shell(name+suffix) + " 2>/dev/null || true)\" = masked\ntest \"$(systemctl show --value -p ActiveState " + shell(name+suffix) + ")\" = inactive\ntest -s " + shell("/var/lib/ebpf-wg-mesh/"+f.id+"/quarantine/units/"+name+suffix) + "\n"
			f.docker("exec", f.hosts["b"], "sh", "-eu", "-c", script)
		}
	}
	for _, endpoint := range target.Endpoints {
		if _, err = probeHTTP(f.ctx, f.selection.EndpointProbes[endpoint.Name], false); err != nil {
			t.Fatal("restored public path", endpoint.Name, err)
		}
	}
	t.Logf("measured native platform restore, siteLoss=%t: %s", siteLoss, time.Since(declared))
	if time.Since(declared) > 2*time.Hour {
		t.Fatal("native platform recovery exceeded objective")
	}
}
