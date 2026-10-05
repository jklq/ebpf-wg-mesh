package main

import (
	"context"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/identity/identitytest"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

// serveXDSEndpoint runs a publishing xDS server on a random loopback port,
// returning its address, a publish function, and a stop function.
func serveXDSEndpoint(ctx context.Context, t *testing.T, pki *identitytest.PKI, backends []xds.Backend) (addr string, publish func([]xds.Backend), stop func()) {
	t.Helper()
	server := xds.NewServer(ctx)
	publish = func(backends []xds.Backend) {
		t.Helper()
		snap, err := xds.Build(context.Background(), xds.BuildInput{Backends: backends, HTTPListenAddrs: []string{":8080"}})
		if err != nil {
			t.Fatal(err)
		}
		server.Publish(ctx, snap)
	}
	publish(backends)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := server.GRPCServer(pki.Authority.XDSConfig(), pki.Authority.Revocations())
	go func() { _ = grpcServer.Serve(listener) }()
	return listener.Addr().String(), publish, func() {
		grpcServer.Stop()
		_ = listener.Close()
	}
}

func waitForSnapshot(t *testing.T, dir string, want probeSnapshot) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var snapshot probeSnapshot
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "latest.json"))
		if err == nil {
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshotContains(snapshot, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never reached %+v: %+v", want, snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForSnapshotWithout(t *testing.T, dir string, gone ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var raw []byte
	for {
		var err error
		raw, err = os.ReadFile(filepath.Join(dir, "latest.json"))
		if err == nil {
			present := false
			for _, resource := range gone {
				if strings.Contains(string(raw), resource) {
					present = true
					break
				}
			}
			if !present {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never dropped %v: %s", gone, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func snapshotContains(snapshot, want probeSnapshot) bool {
	for _, hostname := range want.Hostnames {
		if !listContains(snapshot.Hostnames, hostname) {
			return false
		}
	}
	for _, endpoint := range want.Endpoints {
		if !listContains(snapshot.Endpoints, endpoint) {
			return false
		}
	}
	return true
}

func listContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func allTypesTracked(versions map[string]map[string]string) bool {
	for _, perType := range versions {
		if len(perType) != 4 {
			return false
		}
	}
	return true
}

func TestProbeRecordsUnionAcrossEndpoints(t *testing.T) {
	t.Parallel()

	pki := identitytest.New(t)
	identityDir := t.TempDir()
	if err := xds.WriteIdentity(identityDir, identityDir, pki.Material(t, identity.CallerIngress, "probe-test")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrA, _, stopA := serveXDSEndpoint(ctx, t, pki, []xds.Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}})
	t.Cleanup(stopA)
	addrB, _, stopB := serveXDSEndpoint(ctx, t, pki, []xds.Backend{{Domain: "b.example.com", Upstream: "10.0.0.11:8080"}})
	t.Cleanup(stopB)

	dir := t.TempDir()
	runCtx, stopProbe := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(runCtx, probeConfig{addrs: []string{addrA, addrB}, dir: dir, nodeID: "probe-test", identityDir: identityDir, serverName: "controlplane"})
	}()
	defer func() {
		stopProbe()
		<-runDone
	}()

	deadline := time.Now().Add(10 * time.Second)
	var snapshot probeSnapshot
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "latest.json"))
		if err == nil {
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshotContains(snapshot, probeSnapshot{
				Hostnames: []string{"a.example.com", "b.example.com"},
				Endpoints: []string{"10.0.0.10:8080", "10.0.0.11:8080"},
			}) && len(snapshot.Versions) == 2 && allTypesTracked(snapshot.Versions) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never converged: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for addr, perType := range snapshot.Versions {
		if len(perType) != 4 {
			t.Fatalf("endpoint %s tracked %d types, want 4", addr, len(perType))
		}
	}
	rawLog, err := os.ReadFile(filepath.Join(dir, "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(rawLog), "\n"); lines < 8 {
		t.Fatalf("requests.log has %d lines, want at least 8 (4 types x 2 endpoints)", lines)
	}
}

// TestProbeDropsWithdrawnResources: a response that no longer carries a route or
// endpoint removes it from latest.json instead of accumulating it.
func TestProbeDropsWithdrawnResources(t *testing.T) {
	t.Parallel()

	pki := identitytest.New(t)
	identityDir := t.TempDir()
	if err := xds.WriteIdentity(identityDir, identityDir, pki.Material(t, identity.CallerIngress, "probe-test")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, publish, stop := serveXDSEndpoint(ctx, t, pki, []xds.Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}})
	t.Cleanup(stop)

	dir := t.TempDir()
	runCtx, stopProbe := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(runCtx, probeConfig{addrs: []string{addr}, dir: dir, nodeID: "probe-test", identityDir: identityDir, serverName: "controlplane"})
	}()
	defer func() {
		stopProbe()
		<-runDone
	}()

	waitForSnapshot(t, dir, probeSnapshot{Hostnames: []string{"a.example.com"}, Endpoints: []string{"10.0.0.10:8080"}})
	publish([]xds.Backend{{Domain: "b.example.com", Upstream: "10.0.0.11:8080"}})
	waitForSnapshot(t, dir, probeSnapshot{Hostnames: []string{"b.example.com"}, Endpoints: []string{"10.0.0.11:8080"}})
	waitForSnapshotWithout(t, dir, "a.example.com", "10.0.0.10:8080")
}

// TestProbeDropsResourcesOfDisconnectedEndpoints: a dead endpoint stops
// contributing resources and versions, so presence checks cannot pass on data
// only it advertised.
func TestProbeDropsResourcesOfDisconnectedEndpoints(t *testing.T) {
	t.Parallel()

	pki := identitytest.New(t)
	identityDir := t.TempDir()
	if err := xds.WriteIdentity(identityDir, identityDir, pki.Material(t, identity.CallerIngress, "probe-test")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrA, _, stopA := serveXDSEndpoint(ctx, t, pki, []xds.Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}})
	addrB, _, stopB := serveXDSEndpoint(ctx, t, pki, []xds.Backend{{Domain: "b.example.com", Upstream: "10.0.0.11:8080"}})
	t.Cleanup(stopB)

	dir := t.TempDir()
	runCtx, stopProbe := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(runCtx, probeConfig{addrs: []string{addrA, addrB}, dir: dir, nodeID: "probe-test", identityDir: identityDir, serverName: "controlplane"})
	}()
	defer func() {
		stopProbe()
		<-runDone
	}()

	waitForSnapshot(t, dir, probeSnapshot{Hostnames: []string{"a.example.com", "b.example.com"}})
	stopA()
	waitForSnapshotWithout(t, dir, "a.example.com", "10.0.0.10:8080", addrA)
	waitForSnapshot(t, dir, probeSnapshot{Hostnames: []string{"b.example.com"}, Endpoints: []string{"10.0.0.11:8080"}})
}

func TestRunRejectsEmptyConfig(t *testing.T) {
	t.Parallel()

	if err := run(context.Background(), probeConfig{}); err == nil {
		t.Fatal("expected an error with no addresses")
	}
	if err := run(context.Background(), probeConfig{addrs: []string{"127.0.0.1:1"}}); err == nil {
		t.Fatal("expected an error with no probe dir")
	}
}
