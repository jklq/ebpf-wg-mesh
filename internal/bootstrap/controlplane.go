package bootstrap

import (
	"flag"
	"fmt"

	"ebof-wg-mesh/internal/config"
)

func ControlPlane(args []string) (config.ControlPlaneConfig, error) {
	var cfg config.ControlPlaneConfig
	var bootstrapUsers []bootstrapUserSpec
	var internalServerNames string
	var agentBootstrapTokens string

	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	stringFlag(fs, &cfg.PublicHTTP.Listen, "public-listen", "CONTROLPLANE_PUBLIC_LISTEN", "0.0.0.0:8080", "")
	stringFlag(fs, &cfg.InternalGRPC.Listen, "internal-listen", "CONTROLPLANE_INTERNAL_LISTEN", "0.0.0.0:9443", "")
	stringFlag(fs, &internalServerNames, "internal-server-names", "CONTROLPLANE_INTERNAL_SERVER_NAMES", "controlplane,controlplane-internal,localhost", "")
	stringFlag(fs, &agentBootstrapTokens, "agent-bootstrap-tokens", "CONTROLPLANE_AGENT_BOOTSTRAP_TOKENS", "", "")
	intFlag(fs, &cfg.InternalGRPC.TLS.ServerCertValidityHours, "internal-server-cert-validity-hours", "CONTROLPLANE_INTERNAL_SERVER_CERT_VALIDITY_HOURS", 24*30, "")
	intFlag(fs, &cfg.InternalGRPC.TLS.ClientCertValidityHours, "internal-client-cert-validity-hours", "CONTROLPLANE_INTERNAL_CLIENT_CERT_VALIDITY_HOURS", 24, "")
	stringFlag(fs, &cfg.Database.URL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &cfg.StateDir, "state-dir", "CONTROLPLANE_STATE_DIR", "var/controlplane", "")
	stringFlag(fs, &cfg.OIDC.Issuer, "oidc-issuer", "CONTROLPLANE_OIDC_ISSUER", "", "")
	stringFlag(fs, &cfg.OIDC.Audience, "oidc-audience", "CONTROLPLANE_OIDC_AUDIENCE", "", "")
	stringFlag(fs, &cfg.OIDC.JWKSURL, "oidc-jwks-url", "CONTROLPLANE_OIDC_JWKS_URL", "", "")
	stringFlag(fs, &cfg.OIDC.AllowedEmailDomain, "oidc-allowed-email-domain", "CONTROLPLANE_OIDC_ALLOWED_EMAIL_DOMAIN", "", "")
	stringFlag(fs, &cfg.Ingress.AdminURL, "ingress-admin-url", "CONTROLPLANE_INGRESS_ADMIN_URL", "http://127.0.0.1:2019/load", "")
	stringFlag(fs, &cfg.Ingress.PublicAddr, "ingress-public-addr", "CONTROLPLANE_INGRESS_PUBLIC_ADDR", "platform.local", "")
	stringFlag(fs, &cfg.Ingress.ControlPlaneHTTPUpstream, "ingress-controlplane-upstream", "CONTROLPLANE_INGRESS_CONTROLPLANE_UPSTREAM", "127.0.0.1:8080", "")
	stringFlag(fs, &cfg.Mesh.InterfaceName, "mesh-interface-name", "CONTROLPLANE_MESH_INTERFACE_NAME", "wg0", "")
	intFlag(fs, &cfg.Mesh.ListenPort, "mesh-listen-port", "CONTROLPLANE_MESH_LISTEN_PORT", 51820, "")
	stringFlag(fs, &cfg.Mesh.NetworkCIDR, "mesh-network-cidr", "CONTROLPLANE_MESH_NETWORK_CIDR", "fd00:44::/64", "")
	stringFlag(fs, &cfg.Mesh.WorkloadPoolCIDR, "mesh-workload-pool-cidr", "CONTROLPLANE_MESH_WORKLOAD_POOL_CIDR", "fd00:200::/48", "")
	intFlag(fs, &cfg.Mesh.PersistentKeepaliveSeconds, "mesh-persistent-keepalive-seconds", "CONTROLPLANE_MESH_PERSISTENT_KEEPALIVE_SECONDS", 5, "")
	fs.Var(bootstrapUsersFlag{users: &bootstrapUsers}, "bootstrap-user", "subject:email[:project1,project2]")

	if err := fs.Parse(args); err != nil {
		return config.ControlPlaneConfig{}, err
	}
	cfg.InternalGRPC.TLS.ServerNames = splitCommaList(internalServerNames)
	cfg.InternalGRPC.TLS.BootstrapTokens = splitCommaList(agentBootstrapTokens)
	for _, user := range bootstrapUsers {
		cfg.Bootstrap.Users = append(cfg.Bootstrap.Users, config.BootstrapUser{
			Subject:  user.subject,
			Email:    user.email,
			Projects: append([]string(nil), user.projects...),
		})
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		return config.ControlPlaneConfig{}, fmt.Errorf("bootstrap control plane: %w", err)
	}
	return cfg, nil
}
