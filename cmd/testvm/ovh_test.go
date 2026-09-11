package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ovh/go-ovh/ovh"
)

func TestOVHBudgetBeforeProvisioning(t *testing.T) {
	base := ovhOptions{agents: 2, hourlyRate: .5, budget: 3}
	if cost, err := base.estimate(45 * time.Minute); err != nil || cost != 3 {
		t.Fatalf("cost=%v err=%v", cost, err)
	}
	for _, modify := range []func(*ovhOptions){
		func(o *ovhOptions) { o.agents = 33 }, func(o *ovhOptions) { o.hourlyRate = 0 }, func(o *ovhOptions) { o.hourlyRate = math.NaN() }, func(o *ovhOptions) { o.budget = math.Inf(1) }, func(o *ovhOptions) { o.budget = 2.99 },
	} {
		o := base
		modify(&o)
		if _, err := o.estimate(45 * time.Minute); err == nil {
			t.Fatalf("accepted invalid budget: %+v", o)
		}
	}
	if _, err := base.estimate(7 * time.Hour); err == nil {
		t.Fatal("unbounded run accepted")
	}
}

type fakeOVH struct {
	get  func(string, any) error
	post func(string, any, any) error
	del  func(string) error
}

func (f fakeOVH) GetWithContext(_ context.Context, p string, out any) error { return f.get(p, out) }
func (f fakeOVH) PostWithContext(_ context.Context, p string, in, out any) error {
	return f.post(p, in, out)
}
func (f fakeOVH) DeleteWithContext(_ context.Context, p string, _ any) error { return f.del(p) }
func assignJSON(out, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func TestOVHLostCreateResponseKeepsCleanupOwnership(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "runner")
	if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := fakeOVH{
		get: func(path string, out any) error { return assignJSON(out, []any{}) },
		post: func(path string, in, out any) error {
			if strings.HasSuffix(path, "/sshkey") {
				return assignJSON(out, ovhKey{ID: "key-1", Name: "vm-test-runner"})
			}
			calls++
			body := in.(map[string]any)
			if body["monthlyBilling"] != false {
				t.Fatal("monthly billing enabled")
			}
			if !strings.Contains(body["userData"].(string), "ssh-ed25519 test-key") {
				t.Fatal("root SSH key missing")
			}
			return context.DeadlineExceeded
		},
	}
	_, cleanup, err := provisionOVH(context.Background(), api, ovhPlan{Endpoint: "ovh-ca", Project: "project", Region: "GRA11", RunID: "vm-test", Hosts: 3}, "../..", dir, key)
	if !errors.Is(err, context.DeadlineExceeded) || cleanup == nil || calls != 1 {
		t.Fatalf("err=%v cleanup=%v POSTs=%d", err, cleanup != nil, calls)
	}
	var state ovhResources
	data, err := os.ReadFile(filepath.Join(dir, "ovh-resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Names) != 3 || state.Names[0] != "vm-test-controlplane" || state.SSHKeyID != "key-1" || state.Destroyed {
		t.Fatalf("unrecoverable manifest: %+v", state)
	}
	stat, err := os.Stat(filepath.Join(dir, "ovh-resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("manifest mode %v", stat.Mode())
	}
}

func TestOVHCleanupReconcilesAmbiguousCreatesWithoutDeletingOtherRuns(t *testing.T) {
	instances := []ovhInstance{
		{ID: "lost-response", Name: "vm-test-controlplane", Region: "GRA11", Status: "ACTIVE"},
		{ID: "deleting", Name: "vm-test-agent-01", Region: "GRA11", Status: "DELETING"},
		{ID: "owned-after-error", Name: "vm-test-agent-02", Region: "GRA11", Status: "ERROR"},
		{ID: "unrelated", Name: "production", Region: "GRA11", Status: "ACTIVE"},
		{ID: "other-region", Name: "vm-test-controlplane", Region: "BHS5", Status: "ACTIVE"},
	}
	var deleted []string
	failure := errors.New("transient delete failure")
	api := fakeOVH{
		get: func(_ string, out any) error { return assignJSON(out, instances) },
		del: func(path string) error {
			deleted = append(deleted, path)
			if strings.HasSuffix(path, "lost-response") {
				return failure
			}
			return &ovh.APIError{Code: 404}
		},
	}
	pending, err := reconcileOVHInstances(context.Background(), api, "/cloud/project/p", "GRA11", map[string]bool{"vm-test-controlplane": true, "vm-test-agent-01": true, "vm-test-agent-02": true})
	if !pending || !errors.Is(err, failure) {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	want := []string{"/cloud/project/p/instance/lost-response", "/cloud/project/p/instance/owned-after-error"}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted %v want %v", deleted, want)
	}
}

func TestOVHHostUsesPublicAddressAndIPv4Fallback(t *testing.T) {
	var instance ovhInstance
	err := json.Unmarshal([]byte(`{"name":"agent","ipAddresses":[{"ip":"10.0.0.2","type":"private","version":4},{"ip":"192.0.2.10","type":"public","version":4}]}`), &instance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ovhHost(instance, "agent"); err == nil {
		t.Fatal("IPv4-only instance must be rejected before agent advertise-addr validation")
	}

	var dualStack ovhInstance
	err = json.Unmarshal([]byte(`{"name":"agent","ipAddresses":[{"ip":"10.0.0.2","type":"private","version":4},{"ip":"192.0.2.10","type":"public","version":4},{"ip":"2001:db8::10","type":"public","version":6}]}`), &dualStack)
	if err != nil {
		t.Fatal(err)
	}
	dualHost, err := ovhHost(dualStack, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if dualHost.PublicIPv4 != "192.0.2.10" || dualHost.PublicIPv6 != "2001:db8::10" || hostAdvertiseAddress(dualHost) != "2001:db8::10" {
		t.Fatalf("dual-stack host=%+v", dualHost)
	}
}

func TestDestroyOVHMissingManifestIsNoop(t *testing.T) {
	err := destroyOVH(context.Background(), fakeOVH{}, filepath.Join(t.TempDir(), "missing-ovh-resources.json"))
	if err != nil {
		t.Fatalf("missing manifest: %v", err)
	}
}

func TestOVHTransientClassifiesServerErrors(t *testing.T) {
	for _, code := range []int{408, 425, 429, 500} {
		if !ovhTransient(&ovh.APIError{Code: code}) {
			t.Fatalf("%d must be retried", code)
		}
	}
	if ovhTransient(&ovh.APIError{Code: 404}) || ovhTransient(&ovh.APIError{Code: 401}) {
		t.Fatal("4xx must not be retried as transient")
	}
	if ovhTransient(context.Canceled) || ovhTransient(errors.New("invalid response")) {
		t.Fatal("cancellation and arbitrary local errors must not be retried")
	}
}

func TestWireGuardEndpointUsesReachableProviderUnderlay(t *testing.T) {
	host := hostInfo{PublicIPv4: "192.0.2.10", PublicIPv6: "2001:db8::10"}
	if got := hostWireGuardEndpoint(host, "ovh"); got != "192.0.2.10:51820" {
		t.Fatalf("OVH WireGuard endpoint = %q", got)
	}
	if got := hostWireGuardEndpoint(host, "hetzner"); got != "[2001:db8::10]:51820" {
		t.Fatalf("Hetzner WireGuard endpoint = %q", got)
	}
}
