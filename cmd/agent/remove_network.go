package main

import (
	"flag"
	"fmt"

	"ebof-wg-mesh/internal/mesh"
)

func removeNetwork(args []string) error {
	fs := flag.NewFlagSet("agent remove-network", flag.ContinueOnError)
	iface := fs.String("mesh-interface-name", "wg0", "WireGuard interface and pinned firewall to destroy; stop the agent first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return mesh.Remove(*iface)
}
