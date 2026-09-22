package xds

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// BootstrapConfig describes one Envoy instance's static bootstrap. All
// listeners, routes, clusters, and endpoints arrive over ADS; the bootstrap
// carries only the node identity, the admin interface, and the xDS cluster.
// XDSAddresses lists every control-plane replica's xDS endpoint: the xDS
// endpoint is shared, so an Envoy whose replica dies or is replaced reaches
// the live owner through the remaining addresses.
type BootstrapConfig struct {
	NodeID       string
	XDSAddresses []string
	AdminAddress string
}

// RenderBootstrap renders an Envoy bootstrap YAML that subscribes to the
// control-plane xDS authority over ADS. It fails closed on malformed input
// rather than emitting a config Envoy would reject at startup.
func RenderBootstrap(cfg BootstrapConfig) (string, error) {
	nodeID := strings.TrimSpace(cfg.NodeID)
	if nodeID == "" {
		return "", fmt.Errorf("xds bootstrap node id is required")
	}
	var xdsEndpoints strings.Builder
	for _, address := range cfg.XDSAddresses {
		xdsHost, xdsPort, err := splitHostPort(address, "xds address")
		if err != nil {
			return "", err
		}
		xdsEndpoints.WriteString(fmt.Sprintf(`        - endpoint:
            address:
              socket_address:
                address: %s
                port_value: %d
`, yamlScalar(xdsHost), xdsPort))
	}
	if xdsEndpoints.Len() == 0 {
		return "", fmt.Errorf("xds bootstrap requires at least one xds address")
	}
	adminHost, adminPort, err := splitHostPort(cfg.AdminAddress, "admin address")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`node:
  id: %s
  cluster: ingress
admin:
  address:
    socket_address:
      protocol: TCP
      address: %s
      port_value: %d
dynamic_resources:
  lds_config:
    ads: {}
    resource_api_version: V3
  cds_config:
    ads: {}
    resource_api_version: V3
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    grpc_services:
    - envoy_grpc:
        cluster_name: xds_cluster
static_resources:
  clusters:
  - name: xds_cluster
    connect_timeout: 5s
    type: STRICT_DNS
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    load_assignment:
      cluster_name: xds_cluster
      endpoints:
      - lb_endpoints:
%s`,
		yamlScalar(nodeID),
		yamlScalar(adminHost), adminPort,
		xdsEndpoints.String(),
	), nil
}

func splitHostPort(raw, field string) (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("xds bootstrap %s %q must be host:port: %w", field, raw, err)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", 0, fmt.Errorf("xds bootstrap %s %q must include a host", field, raw)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("xds bootstrap %s %q must use a port between 1 and 65535", field, raw)
	}
	return strings.Trim(host, "[]"), uint32(port), nil
}

func yamlScalar(raw string) string {
	if raw == "" {
		return `""`
	}
	plain := true
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '/':
		default:
			plain = false
		}
	}
	if plain {
		return raw
	}
	return strconv.Quote(raw)
}
