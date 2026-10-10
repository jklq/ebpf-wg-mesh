//go:build !linux

package firewall

import (
	"context"
	"errors"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/meshstate"
)

type Manager struct{}

func Start(_ context.Context, _ config.MeshRuntimeConfig, _ *meshstate.Ownership) (*Manager, error) {
	return nil, errors.New("firewall runtime requires linux")
}

func (m *Manager) Close() error {
	return nil
}

func Remove(string) error { return errors.New("firewall runtime requires linux") }
