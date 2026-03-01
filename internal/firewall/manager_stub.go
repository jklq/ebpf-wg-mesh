//go:build !linux

package firewall

import (
	"context"
	"errors"

	"ebof-wg-mesh/internal/config"
)

type Manager struct{}

func Start(_ context.Context, _ config.Config) (*Manager, error) {
	return nil, errors.New("firewall runtime requires linux")
}

func (m *Manager) Close() error {
	return nil
}
