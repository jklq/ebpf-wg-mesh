package wgmesh

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"networking-rig/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Runtime struct {
	ifName string
	client *wgctrl.Client
}

func Setup(cfg config.WireGuard) (_ *Runtime, retErr error) {
	if existing, err := netlink.LinkByName(cfg.InterfaceName); err == nil {
		if err := netlink.LinkDel(existing); err != nil {
			return nil, fmt.Errorf("delete existing %s: %w", cfg.InterfaceName, err)
		}
	}

	gl := &netlink.GenericLink{
		LinkAttrs: netlink.LinkAttrs{Name: cfg.InterfaceName},
		LinkType:  "wireguard",
	}
	if err := netlink.LinkAdd(gl); err != nil {
		return nil, fmt.Errorf("create wireguard interface %s: %w", cfg.InterfaceName, err)
	}
	cleanupOnFail := true
	defer func() {
		if cleanupOnFail {
			_ = teardownLink(cfg.InterfaceName)
		}
	}()

	link, err := netlink.LinkByName(cfg.InterfaceName)
	if err != nil {
		return nil, fmt.Errorf("lookup created interface %s: %w", cfg.InterfaceName, err)
	}

	for _, cidr := range cfg.Addresses {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return nil, fmt.Errorf("parse address %q: %w", cidr, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			// Address may already exist in fast restart paths.
			if !isAddressExists(err) {
				return nil, fmt.Errorf("addr add %q to %s: %w", cidr, cfg.InterfaceName, err)
			}
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("set %s up: %w", cfg.InterfaceName, err)
	}

	client, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("create wgctrl client: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = client.Close()
		}
	}()

	privateKey, err := wgtypes.ParseKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	peerCfgs := make([]wgtypes.PeerConfig, 0, len(cfg.Peers))
	for _, p := range cfg.Peers {
		pk, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("peer %q parse public key: %w", p.Name, err)
		}
		ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("peer %q parse endpoint: %w", p.Name, err)
		}

		allowed := make([]net.IPNet, 0, len(p.AllowedIPs))
		for _, cidr := range p.AllowedIPs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, fmt.Errorf("peer %q parse allowed IP %q: %w", p.Name, cidr, err)
			}
			allowed = append(allowed, *ipNet)
		}

		peer := wgtypes.PeerConfig{
			PublicKey:  pk,
			Endpoint:   ep,
			AllowedIPs: allowed,
		}
		if p.PersistentKeepaliveS > 0 {
			keep := time.Duration(p.PersistentKeepaliveS) * time.Second
			peer.PersistentKeepaliveInterval = &keep
		}
		peerCfgs = append(peerCfgs, peer)
	}

	listenPort := cfg.ListenPort
	deviceCfg := wgtypes.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
		Peers:        peerCfgs,
	}
	if err := client.ConfigureDevice(cfg.InterfaceName, deviceCfg); err != nil {
		return nil, fmt.Errorf("configure wireguard device %s: %w", cfg.InterfaceName, err)
	}

	for _, p := range cfg.Peers {
		for _, cidr := range p.AllowedIPs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, fmt.Errorf("parse route cidr %q: %w", cidr, err)
			}
			route := netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipNet}
			if err := netlink.RouteReplace(&route); err != nil {
				return nil, fmt.Errorf("route replace %q via %s: %w", cidr, cfg.InterfaceName, err)
			}
		}
	}

	cleanupOnFail = false
	return &Runtime{ifName: cfg.InterfaceName, client: client}, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.client != nil {
		if err := r.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close wgctrl client: %w", err))
		}
	}
	if r.ifName != "" {
		if err := teardownLink(r.ifName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func teardownLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("lookup %s for teardown: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete interface %s: %w", name, err)
	}
	return nil
}

func isAddressExists(err error) bool {
	// Netlink errors are inconsistently typed between kernels/netlink versions.
	errText := err.Error()
	return strings.Contains(errText, "file exists") || strings.Contains(errText, "exists")
}
