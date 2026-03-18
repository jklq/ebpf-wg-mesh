package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"log"
	"os"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
)

type output struct {
	CAPEMB64   string `json:"ca_pem_b64"`
	CertPEMB64 string `json:"cert_pem_b64"`
	KeyPEMB64  string `json:"key_pem_b64"`
}

func main() {
	stateDir := flag.String("state-dir", "", "controlplane state dir")
	callerID := flag.String("caller-id", "dashboard", "client caller id")
	flag.Parse()

	if *stateDir == "" {
		log.Fatal("missing -state-dir")
	}

	authority, err := controlplane.NewTLSAuthority(config.ControlPlaneConfig{
		StateDir: *stateDir,
		InternalGRPC: config.ListenerConfig{
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"controlplane", "localhost"},
				BootstrapTokens:         []string{"unused"},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
	})
	if err != nil {
		log.Fatalf("load controlplane authority: %v", err)
	}

	identity, err := authority.EnsureDashboardClientIdentity(*callerID)
	if err != nil {
		log.Fatalf("ensure client identity: %v", err)
	}

	if err := json.NewEncoder(os.Stdout).Encode(output{
		CAPEMB64:   base64.StdEncoding.EncodeToString(identity.CAPEM),
		CertPEMB64: base64.StdEncoding.EncodeToString(identity.CertPEM),
		KeyPEMB64:  base64.StdEncoding.EncodeToString(identity.KeyPEM),
	}); err != nil {
		log.Fatalf("write output: %v", err)
	}
}
