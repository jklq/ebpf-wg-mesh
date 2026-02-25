package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"networking-rig/internal/config"
	"networking-rig/internal/firewall"
	"networking-rig/internal/wgmesh"

	"github.com/cilium/ebpf/rlimit"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML config")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("networking rig failed", "error", err)
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

	trustCIDRs, err := collectTrustCIDRs(cfg)
	if err != nil {
		return err
	}
	fw, err := firewall.Attach(cfg.WireGuard.InterfaceName, cfg.Firewall, trustCIDRs)
	if err != nil {
		return fmt.Errorf("firewall attach failed: %w", err)
	}
	defer func() {
		if err := fw.Close(); err != nil {
			slog.Error("firewall teardown failed", "error", err)
		}
	}()

	if cfg.Sync.Enabled {
		if err := fw.StartSync(
			cfg.NodeName,
			cfg.Sync.Listen,
			cfg.Sync.AuthKey,
			time.Duration(cfg.Sync.ReplayWindowSeconds)*time.Second,
			cfg.Sync.Peers,
		); err != nil {
			return fmt.Errorf("start state sync: %w", err)
		}
	}

	slog.Info("node online",
		"node", cfg.NodeName,
		"iface", cfg.WireGuard.InterfaceName,
		"peers", len(cfg.WireGuard.Peers),
		"trustCIDRs", len(trustCIDRs),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutdown signal received")
	return nil
}

func collectTrustCIDRs(cfg config.Config) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cfg.WireGuard.Peers)*2)
	for _, peer := range cfg.WireGuard.Peers {
		cidrs := peer.TrustCIDRs
		if len(cidrs) == 0 {
			cidrs = peer.AllowedIPs
		}
		for _, cidr := range cidrs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return nil, fmt.Errorf("parse trust cidr %q for peer %q: %w", cidr, peer.Name, err)
			}
			out = append(out, prefix.Masked())
		}
	}
	return out, nil
}
