package bootstrap

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/config"
)

// Runner is a constructed service application.
type Runner interface {
	Run(context.Context) error
	Close() error
}

// Run bootstraps a service and runs it until interrupted. It exits the
// process on bootstrap, construction, or run failure.
func Run[Cfg any, A Runner](
	args []string,
	component string,
	parse func([]string) (Cfg, error),
	contract func(Cfg) config.StartupContract,
	create func(Cfg) (A, error),
) {
	cfg, err := parse(args)
	if err != nil {
		slog.Error("bootstrap "+component, "error", err)
		os.Exit(1)
	}
	slog.Info(contract(cfg).String())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := create(cfg)
	if err != nil {
		slog.Error("create "+component, "error", err)
		os.Exit(1)
	}
	defer app.Close()
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error(component+" run failed", "error", err)
		os.Exit(1)
	}
}
