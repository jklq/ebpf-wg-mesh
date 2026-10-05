package bootstrap

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"ebof-wg-mesh/internal/controlplane/xds"
)

func RunIngressBootstrap(args []string) error {
	fs := flag.NewFlagSet("controlplane ingress-bootstrap", flag.ContinueOnError)
	var cfg xds.BootstrapConfig
	var addresses string
	fs.StringVar(&cfg.NodeID, "node-id", "", "stable ingress identity; must match the certificate caller ID")
	fs.StringVar(&cfg.IdentityDir, "identity-dir", "", "identity directory as mounted in Envoy")
	fs.StringVar(&cfg.ServerName, "server-name", "", "control plane certificate SAN to verify")
	fs.StringVar(&cfg.AdminAddress, "admin-address", "127.0.0.1:19000", "Envoy admin listener")
	fs.StringVar(&addresses, "xds-addresses", "", "comma-separated xDS replica addresses")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.XDSAddresses = strings.Split(addresses, ",")
	bootstrap, err := xds.RenderBootstrap(cfg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, bootstrap)
	return err
}
