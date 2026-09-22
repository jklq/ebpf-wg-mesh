package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

func TestProbeRecordsUnionAcrossEndpoints(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serve := func(t *testing.T, backends []xds.Backend) string {
		t.Helper()
		server := xds.NewServer(ctx)
		snap, err := xds.Build(xds.BuildInput{Backends: backends, ListenAddrs: []string{":8080"}})
		if err != nil {
			t.Fatal(err)
		}
		server.Publish(ctx, snap)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		grpcServer := server.GRPCServer()
		go func() { _ = grpcServer.Serve(listener) }()
		t.Cleanup(grpcServer.Stop)
		t.Cleanup(func() { _ = listener.Close() })
		return listener.Addr().String()
	}

	addrA := serve(t, []xds.Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}})
	addrB := serve(t, []xds.Backend{{Domain: "b.example.com", Upstream: "10.0.0.11:8080"}})

	dir := t.TempDir()
	runCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(runCtx, probeConfig{addrs: []string{addrA, addrB}, dir: dir, nodeID: "probe-test"})
	}()

	deadline := time.Now().Add(10 * time.Second)
	var snapshot probeSnapshot
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "latest.json"))
		if err == nil {
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Hostnames) >= 2 && len(snapshot.Endpoints) >= 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never converged: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
	joined := strings.Join(snapshot.Hostnames, " ") + " " + strings.Join(snapshot.Endpoints, " ")
	for _, want := range []string{"a.example.com", "b.example.com", "10.0.0.10:8080", "10.0.0.11:8080"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("union missing %q: %+v", want, snapshot)
		}
	}
	if len(snapshot.Versions) != 2 {
		t.Fatalf("versions = %+v, want one entry per endpoint", snapshot.Versions)
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

	stopProbe()
	<-runDone
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
