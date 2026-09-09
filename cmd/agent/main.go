package main

import (
	"os"

	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/config"
)

func main() {
	bootstrap.Run(os.Args[1:], "agent", bootstrap.Agent, config.AgentStartupContract,
		func(cfg config.AgentConfig) (*agent.App, error) {
			return agent.New(cfg)
		})
}
