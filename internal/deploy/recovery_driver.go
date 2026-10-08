package deploy

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

func restorationPlan(p Plan) bool {
	return p.Recovery
}

func (d *SSHDriver) VerifyRecovery(ctx context.Context, p Plan, state State) error {
	if state.LastRestore == nil {
		return fmt.Errorf("restoration has no selected complete recovery point")
	}
	selected := state.LastRestore.Backup
	if d.recoverySelection == selected+"/"+Digest(p.Release) {
		return nil
	}
	c, s, err := recovery.Load(d.RecoveryConfig)
	if err != nil {
		return fmt.Errorf("recovery artifact staging requires independent --recovery-config: %w", err)
	}
	defer clear(s.RecoveryKey)
	object, err := s.ResolvePoint(ctx, c.Storage.Bucket, selected)
	if err != nil {
		return err
	}
	point, err := s.ReadPoint(ctx, object)
	if err != nil {
		return err
	}
	if report := s.Verify(ctx, point, true); !report.Complete {
		return fmt.Errorf("selected point failed independent verification: missing %v; failures %v", report.Missing, report.Failures)
	}
	if err := verifyReleaseInventory(ctx, s, point.Snapshot, point.Dependencies); err != nil {
		return err
	}
	e := Evidence{Backup: object.S3URL(c.Storage.Bucket), DataLossCutoff: point.Snapshot.Timestamp, Point: &point, Object: &object}
	if err := validateRecoveryEvidence(e, p.Installation.ID, p.Installation.Backup.Target, time.Now()); err != nil {
		return err
	}
	if !e.DataLossCutoff.Equal(state.LastRestore.DataLossCutoff) {
		return fmt.Errorf("protected staging point has a different requested cutoff")
	}
	if err := requirePointRelease(ctx, s, point, p.Release); err != nil {
		return err
	}
	d.recoverySelection = selected + "/" + Digest(p.Release)
	d.recoveryPoint = point
	d.recoveryService = recovery.Service{Storage: s.Storage, Prefix: s.Prefix}
	return nil
}

func (d *SSHDriver) uploadRecovered(ctx context.Context, p Plan, h Host, id string, a Artifact, target string) error {
	var selected *recovery.Dependency
	for n := range d.recoveryPoint.Dependencies {
		dep := &d.recoveryPoint.Dependencies[n]
		if dep.Kind == "tool" && dep.ID == id && dep.Digest == "sha256:"+a.SHA256 {
			selected = dep
			break
		}
	}
	if selected == nil || len(selected.Objects) != 1 {
		return fmt.Errorf("selected recovery point lacks pinned release executable %s", id)
	}
	// A surviving host can retain the exact protected executable. Inspect its
	// actual bytes before reusing it; a staging marker cannot establish this.
	if body, err := d.Remote.Run(ctx, p.Installation, h, "test -f "+quote(target)+"\ntest -x "+quote(target)+"\nsha256sum "+quote(target)+"\n"); err == nil {
		fields := strings.Fields(string(body))
		if len(fields) >= 2 && fields[0] == a.SHA256 {
			return nil
		}
	}
	f, err := os.CreateTemp("", "recover-executable-*")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	o := selected.Objects[0]
	if err := d.recoveryService.Storage.Get(ctx, o, f.Name()); err != nil {
		return err
	}
	digest, size, err := recovery.FileDigest(f.Name())
	if err != nil {
		return err
	}
	if digest != o.Digest || size != o.Size || digest != "sha256:"+a.SHA256 {
		return fmt.Errorf("protected release executable %s failed digest verification", id)
	}
	return d.Remote.Upload(ctx, p.Installation, h, f.Name(), target, a.SHA256)
}

func (d *SSHDriver) stageRecovered(ctx context.Context, p Plan, state State, h Host, pl *Placement) (string, error) {
	if err := d.VerifyRecovery(ctx, p, state); err != nil {
		return "", err
	}
	if pl != nil {
		a, ok := p.Release.Programs[pl.Role].Artifacts[h.Architecture]
		if !ok {
			return "", fmt.Errorf("missing recovery program for %s/%s", pl.Role, h.Architecture)
		}
		id := p.Release.ID + "/program/" + string(pl.Role) + "/" + h.Architecture
		if err := d.uploadRecovered(ctx, p, h, id, a, releaseDir(p, *pl)+"/program"); err != nil {
			return "", err
		}
		script := "mkdir -p " + quote(configDir(p.Installation, *pl)) + " " + quote(stateDir(p.Installation, *pl)) + "\n"
		for _, name := range p.Installation.Components[pl.Role].Storage {
			script += "mkdir -p " + quote(p.Installation.Storage[name].Path) + "\n"
		}
		return script, nil
	}
	var names []string
	for name := range p.Release.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a, ok := p.Release.Tools[name][h.Architecture]
		if !ok {
			return "", fmt.Errorf("missing recovery tool for %s/%s", name, h.Architecture)
		}
		id := strings.Join([]string{p.Release.ID, "tool", name, h.Architecture}, "/")
		if err := d.uploadRecovered(ctx, p, h, id, a, toolsDir(p)+"/"+name); err != nil {
			return "", err
		}
	}
	return fileScript(toolsDir(p)+"/staged", []byte(Digest(p.Release.Tools)), "0600"), nil
}

func requirePointRelease(ctx context.Context, s recovery.Service, point recovery.Point, release Release) error {
	for _, dep := range point.Dependencies {
		if dep.Kind != "release" || dep.ID != release.ID || len(dep.Objects) != 1 {
			continue
		}
		f, err := os.CreateTemp("", "recovery-selected-release-*")
		if err != nil {
			return err
		}
		f.Close()
		defer os.Remove(f.Name())
		if err := s.Storage.Get(ctx, dep.Objects[0], f.Name()); err != nil {
			return err
		}
		bundle, err := Load[Release](f.Name())
		if err != nil {
			return err
		}
		if Digest(bundle) != Digest(release) {
			return fmt.Errorf("restore must use the exact release bundle protected by its selected point")
		}
		return nil
	}
	return fmt.Errorf("selected point does not protect the requested recovery release")
}
