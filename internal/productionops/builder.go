package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func (r *Runner) builderConfiguration(pl deploy.Placement) (config.BuilderConfig, error) {
	h, _ := r.Plan.Installation.Host(pl.Host)
	toolchain, frontend := r.Plan.Release.Images["builder-sandbox"], r.Plan.Release.Images["railpack-frontend"]
	if toolchain == "" || frontend == "" || r.Plan.Release.Tools["buildkitd"][h.Architecture].SHA256 == "" {
		return config.BuilderConfig{}, fmt.Errorf("release must package buildkitd and pinned builder-sandbox/railpack-frontend images")
	}
	cfg := config.BuilderConfig{Profile: config.ProfileProduction, ID: pl.Instance, WorkDir: dataDir(r.Plan, pl), AuthorityFile: cfgDir(r.Plan, pl) + "/authority.json", Executor: "hardened", Sandbox: r.Config.BuilderSandbox, RailpackFrontendImage: frontend, RailpackFrontendDirectory: cfgDir(r.Plan, pl) + "/frontend", Health: config.HealthConfig{Listen: "127.0.0.1:9092"}}
	cfg.ControlPlane.Addresses = strings.Split(r.controlPlaneAddresses("9443"), ",")
	cfg.ControlPlane.TLS = config.InternalClientTLSConfig{CAFile: cfgDir(r.Plan, pl) + "/ca.crt", CertFile: cfgDir(r.Plan, pl) + "/client.crt", KeyFile: cfgDir(r.Plan, pl) + "/client.key", ServerName: r.Config.InternalServerName}
	cfg.Sandbox.Image = toolchain
	cfg.Sandbox.BuildkitdBinary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/buildkitd"
	return cfg, config.FinalizeBuilder(&cfg)
}

// Each builder gets its toolchain and frontend from the independent protected
// closure before it starts. Recovery needs neither the publisher nor registry
// credentials from the previous authority.
func (r *Runner) builderImages(ctx context.Context, pl deploy.Placement, verify bool) error {
	cfg, err := r.builderConfiguration(pl)
	if err != nil {
		return err
	}
	configPath := cfgDir(r.Plan, pl) + "/builder-runtime.json"
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	script := remoteFile(configPath, b)
	if verify {
		script = verifyRemoteFile(configPath, b)
	}
	if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
		return err
	}
	_, service, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	defer clear(service.RecoveryKey)
	for name, ref := range map[string]string{"builder-sandbox": cfg.Sandbox.Image, "railpack-frontend": cfg.RailpackFrontendImage} {
		digest := ref[strings.LastIndex(ref, "@")+1:]
		check := shell(operationsBinary(r.Plan)) + " builder-image-verify " + shell(configPath) + " " + shell(ref)
		if name == "railpack-frontend" {
			check = shell(operationsBinary(r.Plan)) + " image-layout-verify " + shell(cfg.RailpackFrontendDirectory) + " " + shell(digest)
		}
		if !verify {
			if _, err := r.remote(ctx, r.Plan, pl, check+"\n"); err == nil {
				continue
			}
		}
		if !verify {
			d, err := service.Find(ctx, recovery.Requirement{Kind: "image", ID: ref, Digest: digest}, time.Now().Add(recovery.Retention))
			if r.Plan.Recovery {
				// A newly protected version must not replace the selected point's
				// exact image closure during destructive recovery.
				e, pointErr := r.selectedPoint(ctx)
				if pointErr != nil {
					return pointErr
				}
				err = fmt.Errorf("selected runtime image is missing")
				for _, candidate := range e.Point.Dependencies {
					if candidate.Kind == "image" && candidate.ID == ref && candidate.Digest == digest {
						d, err = candidate, nil
						break
					}
				}
			}
			if err != nil {
				return err
			}
			local := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "runtime-"+name+".tar")
			if err := service.Materialize(ctx, d, local, false); err != nil {
				return err
			}
			host, err := r.host(ctx, r.Plan, pl.Host)
			if err != nil {
				return err
			}
			remote := cfgDir(r.Plan, pl) + "/runtime-" + name + ".tar"
			hash, _, err := recovery.FileDigest(local)
			if err != nil {
				return err
			}
			if err = r.Remote.Upload(ctx, r.Plan.Installation, host, local, remote, strings.TrimPrefix(hash, "sha256:")); err != nil {
				return err
			}
			command := shell(operationsBinary(r.Plan)) + " builder-image-import " + shell(configPath) + " " + shell(remote) + " " + shell(ref)
			if name == "railpack-frontend" {
				command = shell(operationsBinary(r.Plan)) + " image-layout " + shell(remote) + " " + shell(digest) + " " + shell(cfg.RailpackFrontendDirectory)
			}
			if _, err := r.remote(ctx, r.Plan, pl, command+"\nrm -f "+shell(remote)+"\n"); err != nil {
				return err
			}
			_ = os.Remove(local)
		}
		if _, err := r.remote(ctx, r.Plan, pl, check+"\n"); err != nil {
			return err
		}
	}
	return nil
}
