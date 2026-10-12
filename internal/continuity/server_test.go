//go:build linux

package continuity

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestContinuityHelperProcess(t *testing.T) {
	if os.Getenv("CONTINUITY_HELPER") != "1" {
		return
	}
	address := os.Getenv("HTTP_ADDR")
	if os.Args[len(os.Args)-1] == "stop" {
		if err := os.WriteFile(os.Getenv("STOP_FILE"), []byte(address), 0600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Args[len(os.Args)-1] == "probe" {
		r, err := http.Get("http://" + address + "/ready")
		if err != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	name := os.Getenv("GENERATION")
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("FAIL_READY") == "1" {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(name + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			f, err := os.OpenFile(os.Getenv("DATA_FILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			f.Write(append(b, '\n'))
			f.Sync()
			f.Close()
		}
		w.Write([]byte(name))
	})
	server := http.Server{Addr: address, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() { <-ctx.Done(); server.Close() }()
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		os.Exit(1)
	}
	os.Exit(0)
}

func testRuntime(t *testing.T) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "continuity-test-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(dir)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runtime shutdown stuck")
		}
		os.RemoveAll(dir)
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = Request(ctx, dir, "status", nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return s, addr
}
func testLaunch(t *testing.T, s *Server, addr, name string) Launch {
	t.Helper()
	cfg := filepath.Join(s.dir, "config-"+name)
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "identity"), []byte(name), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := directoryDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := Launch{Command: []string{os.Args[0], "-test.run=TestContinuityHelperProcess", "--", "serve"}, Environment: map[string]string{"CONTINUITY_HELPER": "1", "GENERATION": name, "DATA_FILE": filepath.Join(s.dir, "acknowledged"), "STOP_FILE": filepath.Join(s.dir, "stopped-"+name)}, Directory: s.dir, ConfigDir: cfg, ConfigDigest: digest, Listeners: []Listener{{Address: addr, Env: "HTTP_ADDR"}}, Ready: []string{os.Args[0], "-test.run=TestContinuityHelperProcess", "--", "probe"}, Stop: []string{os.Args[0], "-test.run=TestContinuityHelperProcess", "--", "stop"}, MemoryMiB: 32}
	l.ID = Digest(l)
	return l
}
func getGeneration(client *http.Client, addr string) (string, error) {
	r, err := client.Get("http://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return string(b), err
}

func TestUpgradePreservesConnectionsAndAcknowledgedMutations(t *testing.T) {
	s, addr := testRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	old := testLaunch(t, s, addr, "old")
	if err := s.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	stream, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	if line, _ := reader.ReadString('\n'); line != "old\n" {
		t.Fatal(line)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	var count atomic.Int32
	errors := make(chan error, 1)
	running := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(running)
		for {
			select {
			case <-stop:
				return
			default:
			}
			token := fmt.Sprint(count.Load())
			r, err := client.Post("http://"+addr+"/", "text/plain", strings.NewReader(token))
			if err != nil {
				errors <- err
				return
			}
			b, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil || r.StatusCode != 200 || (string(b) != "old" && string(b) != "new") {
				errors <- fmt.Errorf("failed acknowledged mutation: %v %d %s", err, r.StatusCode, b)
				return
			}
			count.Add(1)
		}
	}()
	for count.Load() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	next := testLaunch(t, s, addr, "new")
	if err = s.Activate(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, err := getGeneration(client, addr); err != nil || got != "new" {
		t.Fatalf("new routing: %s %v", got, err)
	}
	if s.Status().Draining != old.ID {
		t.Fatal("old generation retired before it was drained")
	}
	for count.Load() < 15 {
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	<-running
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "acknowledged"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(b))) != int(count.Load()) {
		t.Fatal("acknowledged writes were lost or replayed")
	}
	if err = s.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("stream outlived its generation")
	}
	if s.Status().Draining != "" {
		t.Fatal("superseded generation still draining")
	}
	b, err = os.ReadFile(filepath.Join(s.dir, "stopped-old"))
	if err != nil || len(b) == 0 || string(b) == addr {
		t.Fatalf("stop preparation did not target the old generation's private listener: %q %v", b, err)
	}
}

func TestFailedCandidateAndUpdaterDisconnectRetainServingGeneration(t *testing.T) {
	s, addr := testRuntime(t)
	ctx := context.Background()
	old := testLaunch(t, s, addr, "old")
	if err := s.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	bad := testLaunch(t, s, addr, "bad")
	bad.Environment["FAIL_READY"] = "1"
	bad.ID = Digest(bad)
	attempt, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if err := s.Activate(attempt, bad); err == nil {
		t.Fatal("unready candidate selected")
	}
	if got, err := getGeneration(http.DefaultClient, addr); err != nil || got != "old" {
		t.Fatalf("serving generation changed: %s %v", got, err)
	}
	next := testLaunch(t, s, addr, "new")
	request, cancelRequest := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { Request(request, s.dir, "activate", &next); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancelRequest()
	<-done
	deadline := time.Now().Add(3 * time.Second)
	for s.Status().Active != next.ID {
		if time.Now().After(deadline) {
			t.Fatal("updater loss cancelled runtime-owned transition")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.RemoveAll(next.ConfigDir); err != nil {
		t.Fatal(err)
	}
	var boot Launch
	b, err := os.ReadFile(filepath.Join(s.dir, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &boot); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryDigest(boot.ConfigDir); err != nil {
		t.Fatal("recovery depends on mutable staging inputs")
	}
}

func TestCandidateExitRestoresOldRoutingAndRetryConverges(t *testing.T) {
	s, addr := testRuntime(t)
	ctx := context.Background()
	old := testLaunch(t, s, addr, "old")
	if err := s.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	next := testLaunch(t, s, addr, "new")
	if err := s.Activate(ctx, next); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	candidate := s.active
	s.mu.Unlock()
	candidate.cmd.Process.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for s.Status().Active != old.ID {
		if time.Now().After(deadline) {
			t.Fatal("candidate failure did not restore healthy old routing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Activate(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, err := getGeneration(http.DefaultClient, addr); err != nil || got != "new" {
		t.Fatalf("retry: %s %v", got, err)
	}
}

func TestExplicitRollbackReusesTheLivingSourceAndDrainsCandidate(t *testing.T) {
	s, addr := testRuntime(t)
	ctx := context.Background()
	old := testLaunch(t, s, addr, "old")
	if err := s.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	before := s.Status().PID
	next := testLaunch(t, s, addr, "new")
	if err := s.Activate(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	if status := s.Status(); status.Active != old.ID || status.PID != before || status.Draining != next.ID {
		t.Fatalf("rollback replaced or lost the live source: %+v", status)
	}
	if got, err := getGeneration(http.DefaultClient, addr); err != nil || got != "old" {
		t.Fatalf("rollback routing: %s %v", got, err)
	}
}
