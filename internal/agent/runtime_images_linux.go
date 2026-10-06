//go:build linux

package agent

import (
	"context"
	"fmt"

	"ebof-wg-mesh/internal/meshlabels"

	"github.com/containerd/containerd/errdefs"
)

// collectUnusedImagesLocked drops platform image roots after their last container
// disappears. Containerd then collects unused content and unpacked snapshots;
// shared layers and live container snapshots remain rooted. imageMu fences pulls
// through container creation so collection cannot race an allocation starting.
func (e *containerdEngine) collectUnusedImagesLocked(ctx context.Context) error {
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return err
	}
	used := make(map[string]bool, len(containers))
	for _, container := range containers {
		info, err := container.Info(ctx)
		if err != nil {
			return err
		}
		used[info.Image] = true
	}
	images, err := e.client.ListImages(ctx, fmt.Sprintf(`labels.%q==true`, meshlabels.Managed))
	if err != nil {
		return err
	}
	for _, image := range images {
		if used[image.Name()] {
			continue
		}
		if err := e.client.ImageService().Delete(ctx, image.Name()); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("collect unused image %s: %w", image.Name(), err)
		}
	}
	return nil
}
