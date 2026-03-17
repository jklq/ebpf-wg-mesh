//go:build linux

package mesh

import (
	"context"
	"errors"
	"fmt"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/firewall"
	"ebof-wg-mesh/internal/wgmesh"
)

type Runtime struct {
	wireguard *wgmesh.Runtime
	firewall  *firewall.Manager
}

func Start(ctx context.Context, cfg config.MeshRuntimeConfig) (*Runtime, error) {
	wgRuntime, err := wgmesh.Setup(cfg.WireGuard)
	if err != nil {
		return nil, fmt.Errorf("wireguard setup: %w", err)
	}
	fw, err := firewall.Start(ctx, cfg)
	if err != nil {
		_ = wgRuntime.Close()
		return nil, fmt.Errorf("firewall start: %w", err)
	}
	return &Runtime{wireguard: wgRuntime, firewall: fw}, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	return errors.Join(r.firewall.Close(), r.wireguard.Close())
}
