//go:build linux

package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/firewall"
	"ebof-wg-mesh/internal/wgmesh"
)

type Runtime struct {
	mu        sync.Mutex
	wireguard *wgmesh.Runtime
	firewall  *firewall.Manager
	closed    bool
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

func (r *Runtime) Update(cfg config.MeshRuntimeConfig) error {
	if r == nil || r.wireguard == nil || r.firewall == nil {
		return errors.New("mesh runtime is not running")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("mesh runtime is closed")
	}
	if err := r.wireguard.Update(cfg.WireGuard); err != nil {
		return fmt.Errorf("wireguard update: %w", err)
	}
	if err := r.firewall.UpdateIdentityCatalog(cfg); err != nil {
		return fmt.Errorf("firewall identity update: %w", err)
	}
	return nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return errors.Join(r.firewall.Close(), r.wireguard.Close())
}
