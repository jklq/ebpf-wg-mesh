package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/bootstrap"
)

func main() {
	cfg, err := bootstrap.Agent(os.Args[1:])
	if err != nil {
		slog.Error("bootstrap agent", "error", err)
		os.Exit(1)
	}
	app, err := agent.New(cfg)
	if err != nil {
		slog.Error("create agent", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent run failed", "error", err)
		os.Exit(1)
	}
}
