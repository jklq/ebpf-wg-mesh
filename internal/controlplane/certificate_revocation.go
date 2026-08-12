package controlplane

import (
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

var errClientCertificateRevoked = errors.New("client certificate is revoked")

// CertificateRevocations is a file-backed denylist of client leaf certificate
// serials. The file is read for every check so an atomic file replacement takes
// effect for new handshakes and RPCs without restarting the control plane.
type CertificateRevocations struct {
	path string
}

func NewCertificateRevocations(path string) (*CertificateRevocations, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("client certificate revocation file is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create client certificate revocation directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open client certificate revocation file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close client certificate revocation file: %w", err)
	}
	revocations := &CertificateRevocations{path: path}
	if _, err := revocations.load(); err != nil {
		return nil, err
	}
	return revocations, nil
}

func (r *CertificateRevocations) Check(cert *x509.Certificate) error {
	if cert == nil || cert.SerialNumber == nil || cert.SerialNumber.Sign() <= 0 {
		return errors.New("client certificate serial is missing")
	}
	serials, err := r.load()
	if err != nil {
		return err
	}
	if _, revoked := serials[cert.SerialNumber.Text(16)]; revoked {
		return errClientCertificateRevoked
	}
	return nil
}

func (r *CertificateRevocations) load() (map[string]struct{}, error) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return nil, fmt.Errorf("read client certificate revocation file: %w", err)
	}
	serials := make(map[string]struct{})
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		serial, err := parseCertificateSerial(line)
		if err != nil {
			return nil, fmt.Errorf("parse client certificate revocation file line %d: %w", lineNumber+1, err)
		}
		serials[serial] = struct{}{}
	}
	return serials, nil
}

func parseCertificateSerial(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	value = strings.ReplaceAll(value, ":", "")
	if value == "" {
		return "", errors.New("empty serial")
	}
	serial := new(big.Int)
	if _, ok := serial.SetString(value, 16); !ok || serial.Sign() <= 0 {
		return "", fmt.Errorf("invalid hexadecimal serial %q", value)
	}
	return serial.Text(16), nil
}
