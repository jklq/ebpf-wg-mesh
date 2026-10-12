//go:build integration && linux

package xds_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/continuity"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/identity/identitytest"
	"ebof-wg-mesh/internal/controlplane/xds"
)

func TestNativeEnvoyReplacementPreservesRoutingAndOpenResponse(t *testing.T) {
	binary := os.Getenv("ENVOY_BINARY")
	if binary == "" {
		binary = "/tmp/compact-bench-repro/envoy"
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skip("set ENVOY_BINARY to a pinned Envoy executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("acknowledged\n"))
		if r.URL.Path == "/hold" {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer backend.Close()
	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return addr
	}
	front, admin := free(), free()
	pki := identitytest.New(t)
	xdsServer := xds.NewServer(ctx)
	snapshot, err := xds.Build(ctx, xds.BuildInput{Static: []xds.StaticRoute{{Hosts: []string{"example.test"}, Upstream: backend.Listener.Addr().String()}}, HTTPListenAddrs: []string{front}})
	if err != nil {
		t.Fatal(err)
	}
	xdsServer.Publish(ctx, snapshot)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := xdsServer.GRPCServer(pki.Authority.XDSConfig(), pki.Authority.Revocations())
	defer grpcServer.Stop()
	go grpcServer.Serve(ln)
	dir, err := os.MkdirTemp("/tmp", "ingress-continuity-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "config")
	if err = os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	ingress := filepath.Join(cfg, "ingress")
	if err = xds.WriteIdentity(ingress, ingress, pki.Material(t, identity.CallerIngress, "ingress-1")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{NodeID: "ingress-1", IdentityDir: ingress, ServerName: "controlplane", XDSAddresses: []string{ln.Addr().String()}, AdminAddress: admin})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cfg, "envoy.yaml"), []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"url": "http://" + admin + "/ready"})
	if err = os.WriteFile(filepath.Join(cfg, "probe.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cfg, "runtime.env"), []byte("GENERATION=\"old\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(dir, "runtime")
	runtime := continuity.New(runtimeDir)
	done := make(chan error, 1)
	runtimeCtx, stop := context.WithCancel(ctx)
	go func() { done <- runtime.Run(runtimeCtx) }()
	defer func() { stop(); <-done }()
	for {
		if _, err = continuity.Request(ctx, runtimeDir, "status", nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("runtime unavailable")
		case <-time.After(10 * time.Millisecond):
		}
	}
	spec := continuity.Launch{Command: []string{binary, "--config-path", filepath.Join(cfg, "envoy.yaml"), "--disable-hot-restart", "--concurrency", "1", "--log-level", "error"}, ConfigDir: cfg, Directory: dir, EnvFiles: []string{filepath.Join(cfg, "runtime.env")}, Listeners: []continuity.Listener{{Address: front, Env: "PLATFORM_ENVOY_HTTP_LISTEN"}, {Address: admin, Env: "PLATFORM_ENVOY_ADMIN_LISTEN"}}, MemoryMiB: 128, Ready: []string{"/usr/bin/python3", "-c", "import json,urllib.request,sys; urllib.request.urlopen(json.load(open(sys.argv[1]))['url']).read()", filepath.Join(cfg, "probe.json")}}
	path := filepath.Join(dir, "launch.json")
	b, _ = json.Marshal(spec)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := continuity.Prepare(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = continuity.Request(ctx, runtimeDir, "activate", &old); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	request := func(path string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+front+path, nil)
		req.Host = "example.test"
		return client.Do(req)
	}
	hold, err := request("/hold")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Body.Close()
	if line, _ := bufio.NewReader(hold.Body).ReadString('\n'); line != "acknowledged\n" {
		t.Fatal(line)
	}
	if err = os.WriteFile(filepath.Join(cfg, "runtime.env"), []byte("GENERATION=\"new\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := continuity.Prepare(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = continuity.Request(ctx, runtimeDir, "activate", &next); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 30; n++ {
		res, err := request("/")
		if err != nil {
			t.Fatal("upgrade interrupted public traffic", err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || string(body) != "acknowledged\n" {
			t.Fatalf("public traffic failed: %d %s %v", res.StatusCode, body, err)
		}
	}
	if _, err = continuity.Request(ctx, runtimeDir, "finish", nil); err == nil {
		t.Fatal("accepted Envoy response was forcibly closed")
	}
	hold.Body.Close()
	for {
		if _, err = continuity.Request(ctx, runtimeDir, "finish", nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
