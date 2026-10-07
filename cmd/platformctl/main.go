package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/deploy"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := deploy.Run(ctx, os.Args[1:], os.Stdout, deploy.NewSSHDriver()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
