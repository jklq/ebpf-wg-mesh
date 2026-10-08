package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

func TestFenceListenerChild(t *testing.T) {
	if os.Getenv("PLATFORM_FENCE_CHILD") != "1" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("PLATFORM_FENCE_ADDRESS"), []byte(listener.Addr().String()), 0600); err != nil {
		os.Exit(3)
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			os.Exit(4)
		}
		connection.Close()
	}
}

func TestRedfishFenceRequiresObservedPowerAndRetriesAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	addressFile := filepath.Join(dir, "address")
	process := exec.Command(os.Args[0], "-test.run=^TestFenceListenerChild$")
	process.Env = append(os.Environ(), "PLATFORM_FENCE_CHILD=1", "PLATFORM_FENCE_ADDRESS="+addressFile)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { process.Wait(); close(exited) }()
	defer func() { process.Process.Kill(); <-exited }()
	var address string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(addressFile); err == nil {
			address = string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if address == "" {
		t.Fatal("isolated fence target did not start")
	}
	var mu sync.Mutex
	resets, stop := 0, false
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		if !ok || user != "administrator" || pass != "isolated-secret" {
			http.Error(w, "denied", 401)
			return
		}
		if req.Method == http.MethodPost {
			var action struct{ ResetType string }
			if json.NewDecoder(req.Body).Decode(&action) != nil || action.ResetType != "ForceOff" {
				http.Error(w, "invalid action", 400)
				return
			}
			mu.Lock()
			resets++
			powerOff := stop
			mu.Unlock()
			if powerOff {
				process.Process.Kill()
				<-exited
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		power := "Off"
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			power = "On"
			connection.Close()
		}
		json.NewEncoder(w).Encode(map[string]any{"PowerState": power, "Actions": map[string]any{"#ComputerSystem.Reset": map[string]string{"Target": "/redfish/v1/Systems/owned/Actions/Reset"}}})
	}))
	defer service.Close()
	credentials, ca := filepath.Join(dir, "credentials.json"), filepath.Join(dir, "ca.crt")
	if err := saveJSON(credentials, map[string]string{"username": "administrator", "password": "isolated-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(ca, pemCertificate(service.Certificate().Raw)); err != nil {
		t.Fatal(err)
	}
	fence := RedfishFence{SystemURL: service.URL + "/redfish/v1/Systems/owned", CredentialsFile: credentials, CAFile: ca}
	r := testRunner(t)
	r.Plan.Previous = &deploy.AppliedDeployment{Installation: r.Plan.Installation, Release: r.Plan.Release, Placements: r.Plan.Placements}
	r.Plan.Recovery = true
	r.Plan.Installation.Hosts = nil // The lost host is not a reused destination.
	r.Config.Fences = map[string]RedfishFence{"a": fence}
	r.Adapter = func(deploy.Installation, deploy.Host) (deploy.Adapter, error) {
		return nil, fmt.Errorf("provider API unavailable")
	}
	if err := fence.off(context.Background()); err == nil {
		t.Fatal("live target reported fenced")
	}
	if err := r.fence(context.Background(), true); err == nil {
		t.Fatal("unavailable provider and live external target reported fenced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := r.fence(ctx, false)
	cancel()
	if err == nil {
		t.Fatal("accepted action without shutdown was counted as fencing")
	}
	if err := fence.off(context.Background()); err == nil {
		t.Fatal("interrupted action hid a live process")
	}
	mu.Lock()
	stop = true
	mu.Unlock()
	if err := r.fence(context.Background(), false); err != nil {
		t.Fatal("retry", err)
	}
	if err := r.fence(context.Background(), true); err != nil {
		t.Fatal("actual shutdown", err)
	}
	if err := r.fence(context.Background(), false); err != nil {
		t.Fatal("idempotent fence", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resets != 2 {
		t.Fatal("verified-off target received a repeated destructive action", resets)
	}
}
