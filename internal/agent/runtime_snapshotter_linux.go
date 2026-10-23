//go:build linux

package agent

import (
	"context"
	"fmt"

	introspection "github.com/containerd/containerd/api/services/introspection/v1"
	containerd "github.com/containerd/containerd/v2/client"
)

func selectSnapshotter(ctx context.Context, client *containerd.Client) (string, error) {
	response, err := client.IntrospectionService().Plugins(ctx, `type==io.containerd.snapshotter.v1`)
	if err != nil {
		return "", fmt.Errorf("inspect containerd snapshotters: %w", err)
	}
	return preferredSnapshotter(response.Plugins)
}

func preferredSnapshotter(plugins []*introspection.Plugin) (string, error) {
	for _, name := range []string{"overlayfs", "native"} {
		for _, plugin := range plugins {
			if plugin.GetType() == "io.containerd.snapshotter.v1" && plugin.GetID() == name && plugin.GetInitErr() == nil {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("containerd has no ready overlayfs or native snapshotter; configure runtime.snapshotter explicitly")
}
