package main

import (
	"context"
	"os"

	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
)

func main() {
	bootstrap.Run(os.Args[1:], "control plane", bootstrap.ControlPlane, config.ControlPlaneStartupContract,
		func(cfg config.ControlPlaneConfig) (*controlplane.Server, error) {
			return controlplane.NewServer(context.Background(), cfg)
		})
}
