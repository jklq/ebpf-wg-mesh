// Command xds-probe subscribes to control-plane xDS endpoints the way an
// Envoy instance would and records what it observes for the VM harness:
// every response is appended to requests.log and the union of hostnames,
// endpoints, and versions across all endpoints lands in latest.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"ebof-wg-mesh/internal/controlplane/xds"
)

var subscribedTypes = []string{
	resourcev3.ListenerType,
	resourcev3.ClusterType,
	resourcev3.RouteType,
	resourcev3.EndpointType,
}

func shortType(typeURL string) string {
	switch typeURL {
	case resourcev3.ListenerType:
		return "lds"
	case resourcev3.ClusterType:
		return "cds"
	case resourcev3.RouteType:
		return "rds"
	case resourcev3.EndpointType:
		return "eds"
	default:
		return typeURL
	}
}

type probeConfig struct {
	addrs  []string
	dir    string
	nodeID string
}

type probe struct {
	cfg probeConfig

	mu        sync.Mutex
	hostnames map[string]struct{}
	endpoints map[string]struct{}
	versions  map[string]map[string]string
}

func main() {
	addrsFlag := flag.String("xds-addrs", "", "comma-separated control-plane xDS addresses (host:port)")
	dirFlag := flag.String("probe-dir", "", "directory for latest.json and requests.log")
	nodeFlag := flag.String("node-id", "testvm-xds-probe", "xDS node identity to subscribe as")
	flag.Parse()

	var addrs []string
	for _, addr := range strings.Split(*addrsFlag, ",") {
		if trimmed := strings.TrimSpace(addr); trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	if len(addrs) == 0 || strings.TrimSpace(*dirFlag) == "" {
		log.Fatal("xds-probe requires -xds-addrs and -probe-dir")
	}
	if err := run(context.Background(), probeConfig{addrs: addrs, dir: *dirFlag, nodeID: *nodeFlag}); err != nil {
		log.Fatalf("xds-probe: %v", err)
	}
}

func run(ctx context.Context, cfg probeConfig) error {
	if len(cfg.addrs) == 0 {
		return fmt.Errorf("at least one xds address is required")
	}
	if strings.TrimSpace(cfg.dir) == "" {
		return fmt.Errorf("probe dir is required")
	}
	if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir probe dir: %w", err)
	}
	p := &probe{
		cfg:       cfg,
		hostnames: make(map[string]struct{}),
		endpoints: make(map[string]struct{}),
		versions:  make(map[string]map[string]string),
	}
	var wg sync.WaitGroup
	for _, addr := range cfg.addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.follow(ctx, addr)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// follow holds one ADS subscription open, resubscribing across control-plane
// restarts and takeovers the way Envoy would.
func (p *probe) follow(ctx context.Context, addr string) {
	backoff := time.Second
	for {
		if err := p.subscribeOnce(ctx, addr); err != nil && ctx.Err() == nil {
			log.Printf("xds-probe %s: %v; resubscribing", addr, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (p *probe) subscribeOnce(ctx context.Context, addr string) error {
	client, err := xds.Dial(ctx, addr, p.cfg.nodeID+"@"+sanitizeAddr(addr))
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	for _, typeURL := range subscribedTypes {
		if err := client.Request(typeURL, "", "", nil); err != nil {
			return err
		}
	}
	for {
		resp, err := client.RecvContext(ctx)
		if err != nil {
			return err
		}
		p.observe(addr, resp)
		if err := client.Request(resp.GetTypeUrl(), resp.GetVersionInfo(), resp.GetNonce(), nil); err != nil {
			return err
		}
	}
}

func (p *probe) observe(addr string, resp *discoveryv3.DiscoveryResponse) {
	hostnames, endpoints := extractResources(resp)
	p.mu.Lock()
	for _, hostname := range hostnames {
		p.hostnames[hostname] = struct{}{}
	}
	for _, endpoint := range endpoints {
		p.endpoints[endpoint] = struct{}{}
	}
	if p.versions[addr] == nil {
		p.versions[addr] = make(map[string]string)
	}
	p.versions[addr][shortType(resp.GetTypeUrl())] = resp.GetVersionInfo()
	snapshot := probeSnapshot{
		Hostnames: sortedKeys(p.hostnames),
		Endpoints: sortedKeys(p.endpoints),
		Versions:  copyVersions(p.versions),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// File writes stay under the mutex: concurrent subscribers share one
	// requests.log and one latest.json staging file.
	line := fmt.Sprintf("%s %s %s resources=%d\n", addr, shortType(resp.GetTypeUrl()), resp.GetVersionInfo(), len(resp.GetResources()))
	if err := appendLine(filepath.Join(p.cfg.dir, "requests.log"), line); err != nil {
		log.Printf("xds-probe %s: append requests.log: %v", addr, err)
	}
	if err := writeLatest(filepath.Join(p.cfg.dir, "latest.json"), snapshot); err != nil {
		log.Printf("xds-probe %s: write latest.json: %v", addr, err)
	}
	p.mu.Unlock()
}

type probeSnapshot struct {
	Hostnames []string                     `json:"hostnames"`
	Endpoints []string                     `json:"endpoints"`
	Versions  map[string]map[string]string `json:"versions"`
	UpdatedAt string                       `json:"updated_at"`
}

func extractResources(resp *discoveryv3.DiscoveryResponse) (hostnames, endpoints []string) {
	switch resp.GetTypeUrl() {
	case resourcev3.RouteType:
		for _, resource := range resp.GetResources() {
			routeConfig := &routev3.RouteConfiguration{}
			if err := resource.UnmarshalTo(routeConfig); err != nil {
				continue
			}
			for _, vhost := range routeConfig.GetVirtualHosts() {
				hostnames = append(hostnames, vhost.GetDomains()...)
			}
		}
	case resourcev3.EndpointType:
		for _, resource := range resp.GetResources() {
			cla := &endpointv3.ClusterLoadAssignment{}
			if err := resource.UnmarshalTo(cla); err != nil {
				continue
			}
			for _, locality := range cla.GetEndpoints() {
				for _, endpoint := range locality.GetLbEndpoints() {
					socket := endpoint.GetEndpoint().GetAddress().GetSocketAddress()
					endpoints = append(endpoints, net.JoinHostPort(socket.GetAddress(), strconv.FormatUint(uint64(socket.GetPortValue()), 10)))
				}
			}
		}
	default:
		// Clusters and listeners carry no probe-observed names; their
		// versions are still recorded in the snapshot.
	}
	return hostnames, endpoints
}

func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		return err
	}
	return file.Sync()
}

func writeLatest(path string, snapshot probeSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o644); err != nil {
		return err
	}
	file, err := os.Open(temporary)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	_ = file.Close()
	return os.Rename(temporary, path)
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func copyVersions(versions map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(versions))
	for addr, perType := range versions {
		dup := make(map[string]string, len(perType))
		for typ, version := range perType {
			dup[typ] = version
		}
		out[addr] = dup
	}
	return out
}

func sanitizeAddr(addr string) string {
	var b strings.Builder
	for _, r := range addr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
