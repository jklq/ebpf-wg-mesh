package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/controlplane"
)

func main() {
	cfg, err := bootstrap.ControlPlane(os.Args[1:])
	if err != nil {
		slog.Error("bootstrap control plane", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server, err := controlplane.NewServer(ctx, cfg)
	if err != nil {
		slog.Error("create control plane", "error", err)
		os.Exit(1)
	}
	defer server.Close()
	if err := server.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("control plane run failed", "error", err)
		os.Exit(1)
	}
}
