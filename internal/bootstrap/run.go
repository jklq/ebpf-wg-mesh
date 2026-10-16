package bootstrap

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/telemetry"
)

type Runner interface {
	Run(context.Context) error
	Close() error
}

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
	// Telemetry is environment-configured (OTEL_* / TELEMETRY_*); setup
	// failure disables export but never blocks the component. The service
	// name is the component name without spaces ("controlplane", "agent",
	// "builder").
	service := strings.ReplaceAll(component, " ", "")
	tcfg := telemetry.ConfigFromEnv(service)
	if os.Getenv("DEPLOYMENT_ENVIRONMENT") == "" && os.Getenv("OTEL_DEPLOYMENT_ENVIRONMENT") == "" {
		tcfg.Environment = string(contract(cfg).Profile)
	}
	tel, terr := telemetry.Setup(context.Background(), tcfg)
	if terr != nil {
		slog.Warn("telemetry setup failed; continuing without export", "component", component, "error", terr)
	}
	slog.Info(contract(cfg).String())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := create(cfg)
	if err != nil {
		slog.Error("create "+component, "error", err)
		flushTelemetry(tel)
		os.Exit(1)
	}
	defer app.Close()
	runErr := app.Run(ctx)
	flushTelemetry(tel)
	if runErr != nil && ctx.Err() == nil {
		slog.Error(component+" run failed", "error", runErr)
		os.Exit(1)
	}
}

func flushTelemetry(tel *telemetry.Provider) {
	if tel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tel.Shutdown(ctx)
}
