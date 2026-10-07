package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func (r *Runner) sourceSelection() (config.SourceArchiveConfig, error) {
	var c config.SourceArchiveConfig
	if r.Config.SourceConfig == "" {
		return c, fmt.Errorf("sourceConfig must select the active source archive service")
	}
	b, err := os.ReadFile(r.Config.SourceConfig)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}
func (r *Runner) restoreArtifacts(ctx context.Context, verify bool) error {
	e, err := r.selectedPoint(ctx)
	if err != nil {
		return err
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	for _, d := range e.Point.Dependencies {
		switch d.Kind {
		case "image":
			if verify {
				if err = c.Images.Verify(ctx, d.ID); err != nil {
					return err
				}
				continue
			}
			repository := strings.Split(d.ID, "@")[0]
			if strings.Split(repository, "/")[0] != r.Config.RegistryService {
				// User-selected external images are verified in place; recovery never
				// writes to an unrelated registry owned by somebody else.
				if err = c.Images.Verify(ctx, d.ID); err != nil {
					return err
				}
			} else if err = s.RestoreImage(ctx, c.Images, d, repository); err != nil {
				return err
			}
		case "source":
			selection, err := r.sourceSelection()
			if err != nil {
				return err
			}
			if selection.Provider == "file" || selection.Provider == "" {
				if len(r.Plan.Installation.Components[deploy.ControlPlane].Storage) == 0 {
					return fmt.Errorf("file archives require declared shared storage")
				}
				for _, pl := range r.Plan.Placements {
					if pl.Role != deploy.ControlPlane {
						continue
					}
					target := filepath.Join(selection.Directory, filepath.FromSlash(d.ID))
					if !strings.HasPrefix(target, selection.Directory+"/") {
						return fmt.Errorf("source escapes backend")
					}
					if !verify {
						local := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "source-transfer")
						if err = s.Materialize(ctx, d, local, false); err != nil {
							return err
						}
						h, err := r.host(ctx, r.Plan, pl.Host)
						if err != nil {
							return err
						}
						if err = r.Remote.Upload(ctx, r.Plan.Installation, h, local, target, strings.TrimPrefix(d.Digest, "sha256:")); err != nil {
							return err
						}
					}
					if _, err = r.remote(ctx, r.Plan, pl, "printf %s "+shell(strings.TrimPrefix(d.Digest, "sha256:")+"  "+target+"\n")+" | sha256sum -c - >/dev/null\n"); err != nil {
						return err
					}
				}
			} else {
				store, err := source.NewSourceArchiveStore(selection)
				if err != nil {
					return err
				}
				if !verify {
					local := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "source-transfer")
					if err = s.Materialize(ctx, d, local, false); err != nil {
						return err
					}
					f, err := os.Open(local)
					if err != nil {
						return err
					}
					info, err := f.Stat()
					if err != nil {
						f.Close()
						return err
					}
					err = store.Put(ctx, d.ID, f, info.Size(), d.Digest)
					f.Close()
					if err != nil {
						return err
					}
				}
				// Read all bytes independently: a metadata digest is not proof of contents.
				f, err := os.CreateTemp(r.Config.StateDirectory, ".source-verify-")
				if err != nil {
					return err
				}
				metadata, err := store.Stat(ctx, d.ID)
				for offset := int64(0); err == nil && offset < metadata.Size; {
					data, e := store.ReadRange(ctx, d.ID, offset, int(min(int64(1<<20), metadata.Size-offset)))
					if e != nil {
						err = e
						break
					}
					if len(data) == 0 {
						err = io.ErrUnexpectedEOF
						break
					}
					_, err = f.Write(data)
					offset += int64(len(data))
				}
				f.Close()
				digest, size, digestErr := recovery.FileDigest(f.Name())
				os.Remove(f.Name())
				if err != nil {
					return err
				}
				if digestErr != nil {
					return digestErr
				}
				if digest != d.Digest || size != metadata.Size {
					return fmt.Errorf("source backend readback differs")
				}
			}
		}
	}
	return nil
}
