package identity

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	pkiDirName         = "pki"
	caCertFileName     = "ca.crt"
	caKeyFileName      = "ca.key"
	serverCertFileName = "server.crt"
	serverKeyFileName  = "server.key"
	clientCertsDirName = "clients"
)

type TLSAuthority struct {
	pkiDir        string
	caCert        *x509.Certificate
	caKey         crypto.Signer
	caPEM         []byte
	serverCert    tls.Certificate
	clientCertTTL time.Duration
	revocations   *CertificateRevocations
}

func NewTLSAuthority(cfg config.ControlPlaneConfig) (*TLSAuthority, error) {
	pkiDir := filepath.Join(cfg.StateDir, pkiDirName)
	if err := os.MkdirAll(pkiDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir pki dir: %w", err)
	}
	revocationFile := strings.TrimSpace(cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile)
	if revocationFile == "" {
		revocationFile = filepath.Join(pkiDir, "revoked-client-cert-serials.txt")
	}
	revocations, err := NewCertificateRevocations(revocationFile)
	if err != nil {
		return nil, err
	}

	caCert, caKey, caPEM, err := loadOrCreateCA(pkiDir)
	if err != nil {
		return nil, err
	}
	serverCert, err := loadOrCreateServerCertificate(
		pkiDir,
		caCert,
		caKey,
		cfg.InternalGRPC.TLS.ServerNames,
		time.Duration(cfg.InternalGRPC.TLS.ServerCertValidityHours)*time.Hour,
	)
	if err != nil {
		return nil, err
	}

	return &TLSAuthority{
		pkiDir:        pkiDir,
		caCert:        caCert,
		caKey:         caKey,
		caPEM:         caPEM,
		serverCert:    serverCert,
		clientCertTTL: time.Duration(cfg.InternalGRPC.TLS.ClientCertValidityHours) * time.Hour,
		revocations:   revocations,
	}, nil
}

func (a *TLSAuthority) HTTPConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(a.caCert)
	return &tls.Config{
		Certificates: []tls.Certificate{a.serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return nil
			}
			return a.revocations.Check(state.PeerCertificates[0])
		},
	}
}

func (a *TLSAuthority) RevokeSerials(serials []string) error {
	if a == nil || a.revocations == nil {
		return errors.New("certificate revocation list is not configured")
	}
	return a.revocations.Add(serials...)
}

func (a *TLSAuthority) Revocations() *CertificateRevocations {
	if a == nil {
		return nil
	}
	return a.revocations
}

func (a *TLSAuthority) PKIDir() string {
	if a == nil {
		return ""
	}
	return a.pkiDir
}

func CertificateSerialFromPEM(raw string) (string, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return "", errors.New("decode issued certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse issued certificate: %w", err)
	}
	if cert.SerialNumber == nil || cert.SerialNumber.Sign() <= 0 {
		return "", errors.New("issued certificate serial is missing")
	}
	return cert.SerialNumber.Text(16), nil
}

func (a *TLSAuthority) Enroll(req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	agentID := strings.TrimSpace(req.GetAgentId())
	if agentID == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id is required")
	}
	csr, err := parseClientCSR(req.GetCsrPem())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(csr.Subject.CommonName) != agentID {
		return nil, status.Error(codes.InvalidArgument, "csr common name must match agent_id")
	}
	return a.issueClientCertificate(CallerAgent, agentID, csr.PublicKey)
}

func (a *TLSAuthority) IssueManagedDashboardCertificate(id, csrPEM string) (*agentv1.EnrollResponse, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, status.Error(codes.FailedPrecondition, "managed dashboard caller id is not configured")
	}
	csr, err := parseClientCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	return a.issueClientCertificate(CallerDashboard, id, csr.PublicKey)
}

func parseClientCSR(raw string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "decode csr")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse csr: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "verify csr: %v", err)
	}
	return csr, nil
}

func (a *TLSAuthority) issueClientCertificate(class CallerClass, id string, publicKey any) (*agentv1.EnrollResponse, error) {
	now := time.Now().UTC()
	notAfter := now.Add(a.clientCertTTL)
	serial, err := randomCertificateSerial()
	if err != nil {
		return nil, status.Error(codes.Internal, "create client certificate serial")
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         id,
			OrganizationalUnit: []string{string(class)},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, a.caCert, publicKey, a.caKey)
	if err != nil {
		return nil, status.Error(codes.Internal, "issue client certificate")
	}
	return &agentv1.EnrollResponse{
		CertPem:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		CaPem:    string(a.caPEM),
		NotAfter: timestamppb.New(notAfter),
	}, nil
}

type ClientIdentityMaterial struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

func (a *TLSAuthority) EnsureClientIdentity(class CallerClass, id string) (ClientIdentityMaterial, error) {
	if strings.TrimSpace(id) == "" {
		return ClientIdentityMaterial{}, errors.New("client identity id is required")
	}
	dir := filepath.Join(a.pkiDir, clientCertsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ClientIdentityMaterial{}, err
	}
	base := string(class) + "-" + strings.TrimSpace(id)
	certPath := filepath.Join(dir, base+".crt")
	keyPath := filepath.Join(dir, base+".key")
	if cert, leaf, err := loadKeyPair(certPath, keyPath); err == nil && time.Now().UTC().Before(leaf.NotAfter.Add(-time.Hour)) {
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return ClientIdentityMaterial{}, err
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return ClientIdentityMaterial{}, err
		}
		_ = cert
		return ClientIdentityMaterial{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: append([]byte(nil), a.caPEM...)}, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("generate client key: %w", err)
	}
	now := time.Now().UTC()
	notAfter := now.Add(a.clientCertTTL)
	serial, err := randomCertificateSerial()
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("create client certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         strings.TrimSpace(id),
			OrganizationalUnit: []string{string(class)},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, a.caCert, key.Public(), a.caKey)
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("issue client certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := marshalPrivateKeyPEM(key)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return ClientIdentityMaterial{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return ClientIdentityMaterial{}, err
	}
	return ClientIdentityMaterial{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		CAPEM:   append([]byte(nil), a.caPEM...),
	}, nil
}

func randomCertificateSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

func (a *TLSAuthority) EnsureDashboardClientIdentity(id string) (ClientIdentityMaterial, error) {
	return a.EnsureClientIdentity(CallerDashboard, id)
}

func (a *TLSAuthority) EnsureBuilderClientIdentity(id string) (ClientIdentityMaterial, error) {
	return a.EnsureClientIdentity(CallerBuilder, id)
}

func loadOrCreateCA(dir string) (*x509.Certificate, crypto.Signer, []byte, error) {
	certPath := filepath.Join(dir, caCertFileName)
	keyPath := filepath.Join(dir, caKeyFileName)
	if cert, caCert, err := loadKeyPair(certPath, keyPath); err == nil {
		signer, ok := cert.PrivateKey.(crypto.Signer)
		if !ok {
			return nil, nil, nil, errors.New("ca private key is not a signer")
		}
		caPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, nil, nil, err
		}
		return caCert, signer, caPEM, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate ca key: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{
			CommonName: "ebof-wg-mesh internal ca",
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create ca cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := marshalPrivateKeyPEM(key)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, err
	}
	return caCert, key, certPEM, nil
}

func loadOrCreateServerCertificate(dir string, caCert *x509.Certificate, caKey crypto.Signer, names []string, validity time.Duration) (tls.Certificate, error) {
	certPath := filepath.Join(dir, serverCertFileName)
	keyPath := filepath.Join(dir, serverKeyFileName)
	if cert, _, err := loadKeyPair(certPath, keyPath); err == nil {
		return cert, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{
			CommonName: firstServerName(names),
		},
		DNSNames:              append([]string(nil), names...),
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, key.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create server cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := marshalPrivateKeyPEM(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

func loadKeyPair(certPath, keyPath string) (tls.Certificate, *x509.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, nil, errors.New("tls keypair missing leaf certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse leaf certificate: %w", err)
	}
	cert.Leaf = leaf
	return cert, leaf, nil
}

func marshalPrivateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func firstServerName(names []string) string {
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return "controlplane"
}

func (a *TLSAuthority) ClusterIdentity() string {
	digest := sha256.Sum256(bytes.TrimSpace(a.caPEM))
	return hex.EncodeToString(digest[:])
}
