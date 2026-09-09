package main

import (
	"os"

	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/builder"
	"ebof-wg-mesh/internal/config"
)

func main() {
	bootstrap.Run(os.Args[1:], "builder", bootstrap.Builder, config.BuilderStartupContract,
		func(cfg config.BuilderConfig) (*builder.App, error) {
			return builder.New(cfg)
		})
}
