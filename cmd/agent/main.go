package main

import (
	"fmt"
	"os"

	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/config"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "remove-network" {
		if err := removeNetwork(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	bootstrap.Run(os.Args[1:], "agent", bootstrap.Agent, config.AgentStartupContract,
		func(cfg config.AgentConfig) (*agent.App, error) {
			return agent.New(cfg)
		})
}
