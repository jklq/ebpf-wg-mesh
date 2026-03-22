package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/bootstrap"
	"ebof-wg-mesh/internal/builder"
)

func main() {
	cfg, err := bootstrap.Builder(os.Args[1:])
	if err != nil {
		slog.Error("bootstrap builder", "error", err)
		os.Exit(1)
	}
	app, err := builder.New(cfg)
	if err != nil {
		slog.Error("create builder", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("builder run failed", "error", err)
		os.Exit(1)
	}
}
