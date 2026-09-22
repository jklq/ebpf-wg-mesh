package xds

import (
	"strings"
	"testing"
)

func TestRenderBootstrap(t *testing.T) {
	t.Parallel()

	out, err := RenderBootstrap(BootstrapConfig{
		NodeID:       "localteststack-envoy",
		XDSAddresses: []string{"host.docker.internal:18000", "10.0.0.2:18000"},
		AdminAddress: "127.0.0.1:19000",
	})
	if err != nil {
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
