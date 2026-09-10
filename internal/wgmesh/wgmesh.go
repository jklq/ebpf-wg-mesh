package wgmesh

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Runtime struct {
	mu     sync.Mutex
	ifName string
	client *wgctrl.Client
	state  wireGuardState
	closed bool
}

func Setup(cfg config.WireGuard) (_ *Runtime, retErr error) {
	state, err := parseWireGuardState(cfg)
	if err != nil {
		return nil, err
	}
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

	for _, addr := range state.addresses {
		address := addr
		if err := netlink.AddrAdd(link, &address); err != nil {
			if !isAddressExists(err) {
				return nil, fmt.Errorf("addr add %q to %s: %w", addr.String(), cfg.InterfaceName, err)
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

	peerCfgs := make([]wgtypes.PeerConfig, 0, len(state.peers))
	for _, peer := range state.peers {
		peerCfgs = append(peerCfgs, peer)
	}

	privateKey := state.privateKey
	listenPort := state.listenPort
	deviceCfg := wgtypes.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
		Peers:        peerCfgs,
	}
	if err := client.ConfigureDevice(cfg.InterfaceName, deviceCfg); err != nil {
		return nil, fmt.Errorf("configure wireguard device %s: %w", cfg.InterfaceName, err)
	}
	if err := installPeerRoutes(link.Attrs().Index, state.routes); err != nil {
		return nil, fmt.Errorf("configure wireguard routes %s: %w", cfg.InterfaceName, err)
	}

	cleanupOnFail = false
	return &Runtime{ifName: cfg.InterfaceName, client: client, state: state}, nil
}

type wireGuardState struct {
	privateKey wgtypes.Key
	listenPort int
	addresses  map[string]netlink.Addr
	peers      map[wgtypes.Key]wgtypes.PeerConfig
	routes     map[string]net.IPNet
}

func parseWireGuardState(cfg config.WireGuard) (wireGuardState, error) {
	privateKey, err := wgtypes.ParseKey(cfg.PrivateKey)
	if err != nil {
		return wireGuardState{}, fmt.Errorf("parse private key: %w", err)
	}
	state := wireGuardState{
		privateKey: privateKey,
		listenPort: cfg.ListenPort,
		addresses:  make(map[string]netlink.Addr, len(cfg.Addresses)),
		peers:      make(map[wgtypes.Key]wgtypes.PeerConfig, len(cfg.Peers)),
		routes:     make(map[string]net.IPNet),
	}
	for _, cidr := range cfg.Addresses {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return wireGuardState{}, fmt.Errorf("parse address %q: %w", cidr, err)
		}
		state.addresses[addr.String()] = *addr
	}
	for _, p := range cfg.Peers {
		peer, err := parsePeer(p)
		if err != nil {
			return wireGuardState{}, err
		}
		if _, exists := state.peers[peer.PublicKey]; exists {
			return wireGuardState{}, fmt.Errorf("duplicate peer public key for %q", p.Name)
		}
		state.peers[peer.PublicKey] = peer
		for _, cidr := range peer.AllowedIPs {
			normalized := normalizeCIDR(cidr)
			state.routes[normalized.String()] = normalized
		}
	}
	return state, nil
}

func parsePeer(p config.PeerConfig) (wgtypes.PeerConfig, error) {
	pk, err := wgtypes.ParseKey(p.PublicKey)
	if err != nil {
		return wgtypes.PeerConfig{}, fmt.Errorf("peer %q parse public key: %w", p.Name, err)
	}
	ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
	if err != nil {
		return wgtypes.PeerConfig{}, fmt.Errorf("peer %q parse endpoint: %w", p.Name, err)
	}
	allowedIPs, err := buildAllowedIPs(p, ep)
	if err != nil {
		return wgtypes.PeerConfig{}, fmt.Errorf("peer %q allowedIPs: %w", p.Name, err)
	}
	keepalive := time.Duration(p.PersistentKeepaliveS) * time.Second
	return wgtypes.PeerConfig{
		PublicKey:                   pk,
		Endpoint:                    ep,
		PersistentKeepaliveInterval: &keepalive,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  allowedIPs,
	}, nil
}

func (r *Runtime) Update(cfg config.WireGuard) error {
	if r == nil {
		return errors.New("wireguard runtime is not running")
	}
	next, err := parseWireGuardState(cfg)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.client == nil {
		return errors.New("wireguard runtime is closed")
	}
	if cfg.InterfaceName != r.ifName {
		return fmt.Errorf("cannot change wireguard interface from %q to %q in place", r.ifName, cfg.InterfaceName)
	}
	link, err := netlink.LinkByName(r.ifName)
	if err != nil {
		return fmt.Errorf("lookup interface %s: %w", r.ifName, err)
	}

	for key, addr := range next.addresses {
		if _, exists := r.state.addresses[key]; exists {
			continue
		}
		address := addr
		if err := netlink.AddrAdd(link, &address); err != nil && !isAddressExists(err) {
			return fmt.Errorf("addr add %q to %s: %w", key, r.ifName, err)
		}
	}

	deviceCfg := deviceUpdate(r.state, next)
	if deviceCfg.PrivateKey != nil || deviceCfg.ListenPort != nil || len(deviceCfg.Peers) > 0 {
		if err := r.client.ConfigureDevice(r.ifName, deviceCfg); err != nil {
			return fmt.Errorf("configure wireguard device %s: %w", r.ifName, err)
		}
	}
	for key, route := range next.routes {
		if _, exists := r.state.routes[key]; exists {
			continue
		}
		if err := installPeerRoute(link.Attrs().Index, route); err != nil {
			return fmt.Errorf("add wireguard route %s: %w", key, err)
		}
	}
	for key, route := range r.state.routes {
		if _, retained := next.routes[key]; retained {
			continue
		}
		if err := deletePeerRoute(link.Attrs().Index, route); err != nil {
			return fmt.Errorf("delete wireguard route %s: %w", key, err)
		}
	}
	for key, addr := range r.state.addresses {
		if _, retained := next.addresses[key]; retained {
			continue
		}
		address := addr
		if err := netlink.AddrDel(link, &address); err != nil && !isAddressNotFound(err) {
			return fmt.Errorf("addr delete %q from %s: %w", key, r.ifName, err)
		}
	}
	r.state = next
	return nil
}

func deviceUpdate(current, next wireGuardState) wgtypes.Config {
	update := wgtypes.Config{}
	if current.privateKey != next.privateKey {
		privateKey := next.privateKey
		update.PrivateKey = &privateKey
	}
	if current.listenPort != next.listenPort {
		listenPort := next.listenPort
		update.ListenPort = &listenPort
	}
	for key := range current.peers {
		if _, retained := next.peers[key]; !retained {
			update.Peers = append(update.Peers, wgtypes.PeerConfig{PublicKey: key, Remove: true})
		}
	}
	for key, peer := range next.peers {
		if existing, exists := current.peers[key]; exists && reflect.DeepEqual(existing, peer) {
			continue
		}
		update.Peers = append(update.Peers, peer)
	}
	return update
}

func buildAllowedIPs(p config.PeerConfig, ep *net.UDPAddr) ([]net.IPNet, error) {
	if len(p.AllowedIPs) > 0 {
		allowed := make([]net.IPNet, 0, len(p.AllowedIPs))
		for _, cidr := range p.AllowedIPs {
			_, parsed, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, fmt.Errorf("parse %q: %w", cidr, err)
			}
			allowed = append(allowed, *parsed)
		}
		return allowed, nil
	}

	if ep == nil || ep.IP == nil {
		return nil, errors.New("endpoint ip is required when allowedIPs are omitted")
	}
	if v6 := ep.IP.To16(); v6 != nil {
		return []net.IPNet{{
			IP:   v6,
			Mask: net.CIDRMask(128, 128),
		}}, nil
	}
	return nil, fmt.Errorf("unsupported endpoint ip %q", ep.IP.String())
}

func installPeerRoutes(linkIndex int, cidrs map[string]net.IPNet) error {
	for key, cidr := range cidrs {
		if err := installPeerRoute(linkIndex, cidr); err != nil {
			return fmt.Errorf("replace route %s: %w", key, err)
		}
	}
	return nil
}

func installPeerRoute(linkIndex int, cidr net.IPNet) error {
	normalized := normalizeCIDR(cidr)
	ones, bits := normalized.Mask.Size()
	if normalized.IP == nil || normalized.Mask == nil || ones == 0 && (bits == 32 || bits == 128) {
		return nil
	}
	dst := normalized
	return netlink.RouteReplace(&netlink.Route{LinkIndex: linkIndex, Dst: &dst})
}

func deletePeerRoute(linkIndex int, cidr net.IPNet) error {
	normalized := normalizeCIDR(cidr)
	ones, bits := normalized.Mask.Size()
	if normalized.IP == nil || normalized.Mask == nil || ones == 0 && (bits == 32 || bits == 128) {
		return nil
	}
	dst := normalized
	err := netlink.RouteDel(&netlink.Route{LinkIndex: linkIndex, Dst: &dst})
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

func normalizeCIDR(cidr net.IPNet) net.IPNet {
	return net.IPNet{IP: cidr.IP.Mask(cidr.Mask), Mask: cidr.Mask}
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
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
	errText := err.Error()
	return strings.Contains(errText, "file exists") || strings.Contains(errText, "exists")
}

func isAddressNotFound(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENOENT)
}
