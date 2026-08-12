//go:build !linux

package mesh

import (
	"context"
	"errors"

	"ebof-wg-mesh/internal/config"
)

type Runtime struct{}

func Start(_ context.Context, _ config.MeshRuntimeConfig) (*Runtime, error) {
	return nil, errors.New("mesh runtime requires linux")
}

func (r *Runtime) Update(config.MeshRuntimeConfig) error {
	return errors.New("mesh runtime requires linux")
}

func (r *Runtime) Close() error {
	return nil
}
