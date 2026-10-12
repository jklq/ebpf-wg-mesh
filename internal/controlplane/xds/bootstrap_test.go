package xds

import (
	"encoding/json"
	"strings"
	"testing"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

func TestRenderBootstrap(t *testing.T) {
	t.Parallel()

	out, err := RenderBootstrap(BootstrapConfig{
		NodeID:      "localteststack-envoy",
		IdentityDir: "/etc/envoy/identity", ServerName: "controlplane",
		XDSAddresses: []string{"host.docker.internal:18000", "10.0.0.2:18000"},
		AdminAddress: "127.0.0.1:19000",
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(raw, &bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.ValidateAll(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"id: localteststack-envoy",
		"cluster_name: xds_cluster",
		"address: host.docker.internal",
		"port_value: 18000",
		"address: 10.0.0.2",
		"address: 127.0.0.1",
		"port_value: 19000",
		"transport_api_version: V3",
		"lds_config:",
		"cds_config:",
		"typed_extension_protocol_options:",
		"tls_minimum_protocol_version: TLSv1_3",
		"max_requests_per_connection: 1",
		"exact: controlplane",
		"path: /etc/envoy/identity/client-sds.json",
		"path: /etc/envoy/identity/ca-sds.json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("bootstrap missing %q:\n%s", want, out)
		}
	}
	// Both xDS endpoints land in the cluster: an Envoy whose replica dies
	// reaches the live owner through the surviving address.
	if got := strings.Count(out, "        - endpoint:"); got != 2 {
		t.Fatalf("xds cluster endpoints = %d, want 2:\n%s", got, out)
	}
	if strings.Contains(out, "static_resources:\n  listeners:") || strings.Contains(out, "route_config_name") {
		t.Fatal("bootstrap must not carry static listeners or routes; those arrive over ADS")
	}
}

func TestRenderBootstrapRejectsBadInput(t *testing.T) {
	t.Parallel()

	for _, cfg := range []BootstrapConfig{
		{},
		{NodeID: "envoy", XDSAddresses: []string{"no-port"}, AdminAddress: "127.0.0.1:19000"},
		{NodeID: "envoy", XDSAddresses: []string{"host:0"}, AdminAddress: "127.0.0.1:19000"},
		{NodeID: "envoy", XDSAddresses: []string{"host:18000", "bad"}, AdminAddress: "127.0.0.1:19000"},
		{NodeID: "envoy", XDSAddresses: []string{"host:18000"}, AdminAddress: ":19000"},
		{NodeID: "", XDSAddresses: []string{"host:18000"}, AdminAddress: "127.0.0.1:19000"},
	} {
		if _, err := RenderBootstrap(cfg); err == nil {
			t.Fatalf("config %+v: expected error", cfg)
		}
	}
}

func TestRenderBootstrapHealthListenerWithdrawsThroughAdmin(t *testing.T) {
	t.Parallel()
	out, err := RenderBootstrap(BootstrapConfig{
		NodeID: "ingress-a", IdentityDir: "/etc/envoy/identity", ServerName: "controlplane",
		XDSAddresses: []string{"10.0.0.2:18000"}, AdminAddress: "127.0.0.1:19000", HealthAddress: "0.0.0.0:19001",
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(raw, &bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.ValidateAll(); err != nil {
		t.Fatal(err)
	}
	listeners := bootstrap.GetStaticResources().GetListeners()
	if len(listeners) != 1 || listeners[0].GetName() != "ingress_health" || listeners[0].GetAddress().GetSocketAddress().GetPortValue() != 19001 {
		t.Fatalf("health listener = %v", listeners)
	}
	if !strings.Contains(out, "pass_through_mode: false") || !strings.Contains(out, "exact: /healthz") {
		t.Fatalf("health check filter does not answer /healthz itself:\n%s", out)
	}
	if _, err := RenderBootstrap(BootstrapConfig{NodeID: "a", IdentityDir: "/i", ServerName: "s", XDSAddresses: []string{"h:1"}, AdminAddress: "127.0.0.1:19000", HealthAddress: "19001"}); err == nil {
		t.Fatal("health address without a host accepted")
	}
}
