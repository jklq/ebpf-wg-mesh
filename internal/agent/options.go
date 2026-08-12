package agent

import (
	"context"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/mesh"
)

type MeshHandle interface {
	Update(config.MeshRuntimeConfig) error
	Close() error
}

type MeshFactory func(context.Context, config.MeshRuntimeConfig) (MeshHandle, error)

type Option func(*options)

type runtimeFactory func(config.AgentConfig) (Runtime, error)

type options struct {
	runtimeFactory runtimeFactory
	meshFactory    MeshFactory
}

func defaultOptions() options {
	return options{
		runtimeFactory: func(cfg config.AgentConfig) (Runtime, error) {
			return NewContainerdRuntime(cfg)
		},
		meshFactory: func(ctx context.Context, cfg config.MeshRuntimeConfig) (MeshHandle, error) {
			return mesh.Start(ctx, cfg)
		},
	}
}

func WithRuntime(runtime Runtime) Option {
	return func(o *options) {
		if runtime == nil {
			return
		}
		o.runtimeFactory = func(config.AgentConfig) (Runtime, error) {
			return runtime, nil
		}
	}
}

func WithRuntimeFactory(factory func(config.AgentConfig) (Runtime, error)) Option {
	return func(o *options) {
		if factory == nil {
			return
		}
		o.runtimeFactory = factory
	}
}

func WithMeshFactory(factory MeshFactory) Option {
	return func(o *options) {
		if factory == nil {
			return
		}
		o.meshFactory = factory
	}
}

func WithMeshDisabled() Option {
	return WithMeshFactory(func(context.Context, config.MeshRuntimeConfig) (MeshHandle, error) {
		return noopMeshHandle{}, nil
	})
}

type noopMeshHandle struct{}

func (noopMeshHandle) Update(config.MeshRuntimeConfig) error {
	return nil
}

func (noopMeshHandle) Close() error {
	return nil
}
