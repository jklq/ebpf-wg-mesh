// operations is the release-owned, host-admin production lifecycle executable.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/productionops"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := productionops.Run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
