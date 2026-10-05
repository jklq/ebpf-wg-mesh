package xds

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ebof-wg-mesh/internal/controlplane/identity"
)

// WriteIdentity installs a complete credential generation and switches current
// atomically. Envoy's filesystem SDS watches the parent of this symlink, so
// certificates, private keys, and CA bundles rotate together without a restart.
// SDS paths are relative to dir's mounted location, which may differ on the host.
func WriteIdentity(dir, mountedDir string, material identity.ClientIdentityMaterial) error {
	if _, err := tls.X509KeyPair(material.CertPEM, material.KeyPEM); err != nil {
		return fmt.Errorf("xDS identity: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(material.CAPEM) {
		return fmt.Errorf("xDS identity has no trust bundle")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	generation, err := os.MkdirTemp(dir, "generation-")
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(generation)
		}
	}()
	for name, data := range map[string][]byte{"client.crt": material.CertPEM, "client.key": material.KeyPEM, "ca.crt": material.CAPEM} {
		if err := os.WriteFile(filepath.Join(generation, name), data, 0o600); err != nil {
			return err
		}
	}
	base := filepath.Join(mountedDir, "current")
	source := func(name string) map[string]string { return map[string]string{"filename": filepath.Join(base, name)} }
	for file, secret := range map[string]map[string]any{
		"client-sds.json": {"name": "xds-client", "tls_certificate": map[string]any{"certificate_chain": source("client.crt"), "private_key": source("client.key"), "watched_directory": map[string]string{"path": mountedDir}}},
		"ca-sds.json":     {"name": "xds-ca", "validation_context": map[string]any{"trusted_ca": source("ca.crt"), "watched_directory": map[string]string{"path": mountedDir}}},
	} {
		secret["@type"] = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
		data, err := json.Marshal(map[string]any{"resources": []any{secret}})
		if err != nil {
			return err
		}
		if err := atomicFile(filepath.Join(dir, file), data); err != nil {
			return err
		}
	}
	link := generation + "-link"
	if err := os.Symlink(filepath.Base(generation), link); err != nil {
		return err
	}
	defer os.Remove(link)
	if err := os.Rename(link, filepath.Join(dir, "current")); err != nil {
		return err
	}
	installed = true
	return nil
}

func atomicFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sds-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// ClientTLS loads the current generation on each reconnect (the probe shares
// the same provisioned credentials as Envoy). Server identity verification is
// mandatory even when dialing an IP address or multiple replica endpoints.
func ClientTLS(dir, serverName string) (*tls.Config, error) {
	if dir == "" || serverName == "" {
		return nil, fmt.Errorf("xDS identity directory and server name are required")
	}
	generation, err := filepath.EvalSymlinks(filepath.Join(dir, "current"))
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(generation, "client.crt"), filepath.Join(generation, "client.key"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(generation, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("xDS trust bundle is invalid")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}, nil
}
