//go:build linux

package mesh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/firewall"
	"ebof-wg-mesh/internal/meshstate"
	"ebof-wg-mesh/internal/wgmesh"
)

type Runtime struct {
	mu        sync.Mutex
	wireguard *wgmesh.Runtime
	firewall  *firewall.Manager
	closed    bool
}

func Start(ctx context.Context, cfg config.MeshRuntimeConfig) (_ *Runtime, retErr error) {
	ownership, err := meshstate.Acquire(cfg.WireGuard.InterfaceName)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = ownership.Close()
		}
	}()
	wgRuntime, err := wgmesh.Setup(cfg.WireGuard)
	if err != nil {
		return nil, fmt.Errorf("wireguard setup: %w", err)
	}
	fw, err := firewall.Start(ctx, cfg, ownership)
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

// Remove is the explicit destructive counterpart to Close. Stop the agent and
// remove its workloads first; removal deliberately drops network connectivity.
func Remove(interfaceName string) error {
	ownership, err := meshstate.Acquire(interfaceName)
	if err != nil {
		return err
	}
	defer ownership.Close()
	if err := os.RemoveAll(ownership.PinDir); err != nil {
		return err
	}
	return wgmesh.Remove(interfaceName)
}
