package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/firewall"
	"ebof-wg-mesh/internal/wgmesh"

	"github.com/cilium/ebpf/rlimit"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML config")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("mesh daemon failed", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock rlimit: %w", err)
	}

	wgRuntime, err := wgmesh.Setup(cfg.WireGuard)
	if err != nil {
		return err
	}
	defer func() {
		if err := wgRuntime.Close(); err != nil {
			slog.Error("wireguard teardown failed", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fw, err := firewall.Start(ctx, cfg)
	if err != nil {
		return fmt.Errorf("firewall bootstrap failed: %w", err)
	}
	defer func() {
		if err := fw.Close(); err != nil {
			slog.Error("firewall teardown failed", "error", err)
		}
	}()

	slog.Info("node online",
		"node", cfg.NodeName,
		"iface", cfg.WireGuard.InterfaceName,
		"peers", len(cfg.WireGuard.Peers),
		"containerd", cfg.Containerd.Socket,
	)

	<-ctx.Done()
	slog.Info("shutdown signal received")
	return nil
}
