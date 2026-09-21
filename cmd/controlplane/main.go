package main

import (
	"context"
	"fmt"
	"os"

	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		if err := bootstrap.RunKeys(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	bootstrap.Run(os.Args[1:], "control plane", bootstrap.ControlPlane, config.ControlPlaneStartupContract,
		func(cfg config.ControlPlaneConfig) (*controlplane.Server, error) {
			return controlplane.NewServer(context.Background(), cfg)
		})
}
