//go:build linux

package builder

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ebof-wg-mesh/internal/config"
	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
)

// ImportRuntimeImage consumes a complete protected OCI archive, rather than
// depending on an external registry being available during site recovery.
func ImportRuntimeImage(ctx context.Context, cfg config.BuilderConfig, archive, ref string) error {
	parts := strings.Split(ref, "@")
	if len(parts) != 2 || digest.Digest(parts[1]).Validate() != nil {
		return fmt.Errorf("runtime image requires a pinned digest")
	}
	c, err := containerd.New(cfg.Sandbox.Socket)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx = namespaces.WithNamespace(ctx, cfg.Sandbox.Namespace)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = c.Import(ctx, f, containerd.WithAllPlatforms(true), containerd.WithDigestRef(func(d digest.Digest) string { return parts[0] + "@" + d.String() })); err != nil {
		return fmt.Errorf("import protected runtime image: %w", err)
	}
	image, err := c.GetImage(ctx, ref)
	if err != nil {
		return err
	}
	if image.Target().Digest.String() != parts[1] {
		return fmt.Errorf("imported runtime image changed its digest")
	}
	return image.Unpack(ctx, cfg.Sandbox.Snapshotter)
}

func VerifyRuntimeImage(ctx context.Context, cfg config.BuilderConfig, ref string) error {
	c, err := containerd.New(cfg.Sandbox.Socket)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx = namespaces.WithNamespace(ctx, cfg.Sandbox.Namespace)
	image, err := c.GetImage(ctx, ref)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(ref, "@"+image.Target().Digest.String()) {
		return fmt.Errorf("local runtime image differs from pinned digest")
	}
	complete, _, _, _, err := images.Check(ctx, c.ContentStore(), image.Target(), platforms.Default())
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("local runtime image has missing content")
	}
	unpacked, err := image.IsUnpacked(ctx, cfg.Sandbox.Snapshotter)
	if err != nil {
		return err
	}
	if !unpacked {
		return fmt.Errorf("local runtime image has not been unpacked")
	}
	return nil
}
