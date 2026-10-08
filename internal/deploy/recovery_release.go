package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

// Enumerate release contents from the protected release bundle, rather than
// trusting a hand-maintained list to remember each architecture and tool.
func releaseRequirements(c recovery.Config) ([]recovery.Requirement, map[string]Artifact, error) {
	artifacts := map[string]Artifact{}
	var needs []recovery.Requirement
	for _, file := range c.Files {
		if file.Requirement.Kind != "release" {
			continue
		}
		r, err := Load[Release](file.Path)
		if err != nil {
			return nil, nil, err
		}
		if r.ID != file.Requirement.ID || r.Version != 1 || r.Configuration <= 0 || r.Protocol <= 0 || r.Schema <= 0 || r.ConsoleSchema <= 0 {
			return nil, nil, fmt.Errorf("release recovery identity does not match its bundle")
		}
		add := func(id string, a Artifact) {
			artifacts[id] = a
			needs = append(needs, recovery.Requirement{Kind: "tool", ID: id, Digest: "sha256:" + a.SHA256})
		}
		for role, p := range r.Programs {
			for arch, a := range p.Artifacts {
				add(r.ID+"/program/"+string(role)+"/"+arch, a)
			}
		}
		for name, architectures := range r.Tools {
			for arch, a := range architectures {
				add(r.ID+"/tool/"+name+"/"+arch, a)
			}
		}
		for _, ref := range r.Images {
			parts := strings.Split(ref, "@")
			if len(parts) != 2 {
				return nil, nil, fmt.Errorf("release image is not digest-pinned")
			}
			needs = append(needs, recovery.Requirement{Kind: "image", ID: ref, Digest: parts[1]})
		}
	}
	return needs, artifacts, nil
}

func protectRelease(ctx context.Context, s recovery.Service, c recovery.Config, verify bool) ([]recovery.Requirement, error) {
	needs, artifacts, err := releaseRequirements(c)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("recovery executable requires HTTPS without excessive redirects")
		}
		return nil
	}}
	for _, r := range needs {
		if _, err := s.Find(ctx, r, time.Now().Add(recovery.Retention)); err == nil {
			continue
		}
		if verify {
			return nil, fmt.Errorf("missing protected release executable %s", r.ID)
		}
		if r.Kind == "image" {
			if _, err := s.ProtectImage(ctx, c.Images, r.ID); err != nil {
				return nil, err
			}
			continue
		}
		a := artifacts[r.ID]
		if !strings.HasPrefix(a.URL, "https://") || len(a.SHA256) != 64 {
			return nil, fmt.Errorf("recovery executables require pinned HTTPS artifacts")
		}
		req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download release recovery executable: %w", err)
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return nil, fmt.Errorf("download recovery executable: HTTP %d", response.StatusCode)
		}
		f, err := os.CreateTemp("", "recovery-release-*")
		if err != nil {
			response.Body.Close()
			return nil, err
		}
		_, copyErr := io.Copy(f, response.Body)
		response.Body.Close()
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(f.Name())
			return nil, fmt.Errorf("stage release recovery executable failed")
		}
		_, err = s.Protect(ctx, r, f.Name(), false, nil)
		os.Remove(f.Name())
		if err != nil {
			return nil, err
		}
	}
	return needs, nil
}

// Validate the bundle's actual schema and executable closure against historical
// installer metadata. Conversion can precede inventory registration; that brief
// window must not publish a point containing only the previous schema's tools.
func verifyReleaseInventory(ctx context.Context, s recovery.Service, snapshot recovery.Snapshot, dependencies []recovery.Dependency) error {
	requirements := map[string]string{}
	for _, r := range snapshot.Requirements {
		requirements[r.Kind+"/"+r.ID] = r.Digest
	}
	matchingSchema := false
	for _, r := range snapshot.Requirements {
		if r.Kind != "release" {
			continue
		}
		var d recovery.Dependency
		if dependencies == nil {
			found, err := s.Find(ctx, r, snapshot.Timestamp.Add(recovery.Retention))
			if err != nil {
				return err
			}
			d = found
		} else {
			for _, dep := range dependencies {
				if dep.Kind == r.Kind && dep.ID == r.ID {
					d = dep
					break
				}
			}
		}
		if len(d.Objects) != 1 {
			return fmt.Errorf("missing protected release bundle %s", r.ID)
		}
		f, err := os.CreateTemp("", "verify-recovery-release-*")
		if err != nil {
			return err
		}
		f.Close()
		err = s.Storage.Get(ctx, d.Objects[0], f.Name())
		if err != nil {
			os.Remove(f.Name())
			return err
		}
		digest, size, err := recovery.FileDigest(f.Name())
		if err != nil || digest != d.Objects[0].Digest || size != d.Objects[0].Size {
			os.Remove(f.Name())
			return fmt.Errorf("protected release bundle failed content verification")
		}
		bundle, err := Load[Release](f.Name())
		if err != nil {
			os.Remove(f.Name())
			return err
		}
		needs, _, err := releaseRequirements(recovery.Config{Files: []recovery.File{{Requirement: r, Path: f.Name()}}})
		os.Remove(f.Name())
		if err != nil {
			return err
		}
		for _, need := range needs {
			if digest, ok := requirements[need.Kind+"/"+need.ID]; !ok || digest != need.Digest {
				return fmt.Errorf("release executable %s missing from timestamped recovery inventory", need.ID)
			}
		}
		if bundle.Schema == snapshot.Schema && bundle.ConsoleSchema == snapshot.ConsoleSchema {
			matchingSchema = true
		}
	}
	if !matchingSchema {
		return fmt.Errorf("no protected release bundle matches the database timestamp's platform and console schemas")
	}
	return nil
}
