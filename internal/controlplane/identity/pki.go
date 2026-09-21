package identity

import (
	"bytes"
	"context"
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
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/signkeys"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	pkiDirName         = "pki"
	serverCertFileName = "server.crt"
	serverKeyFileName  = "server.key"
	clientCertsDirName = "clients"
	// legacyCAFileNames are the pre-2.3b file-based CA. The CA lives in
	// shared signing keys now; NewTLSAuthority removes these so a stale
	// file is never mistaken for authority.
	legacyCACertFileName = "ca.crt"
	legacyCAKeyFileName  = "ca.key"
)

// handshakeKeyTimeout bounds the signing-key reads behind a TLS handshake.
// Handshakes fail closed when key state is unreachable.
const handshakeKeyTimeout = 5 * time.Second

// TLSAuthority issues and verifies internal mTLS identities. The CA key is
// shared signing-key state: every replica signs with the active key and
// verifies against the active plus retiring keys, so rotation never breaks
// issuance or verification. The state directory holds only node-local cache
// (this replica's server leaf and recently issued client identities), never
// authority.
type TLSAuthority struct {
	pkiDir         string
	keys           signkeys.Provider
	serverNames    []string
	serverValidity time.Duration
	clientCertTTL  time.Duration
	revocations    *CertificateRevocations

	mu         sync.RWMutex
	serverCert tls.Certificate
	serverLeaf *x509.Certificate
}

func NewTLSAuthority(ctx context.Context, cfg config.ControlPlaneConfig, keys signkeys.Provider) (*TLSAuthority, error) {
	if keys == nil {
		return nil, errors.New("tls authority requires a signing-key provider")
	}
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
	removeLegacyCAFiles(pkiDir)

	authority := &TLSAuthority{
		pkiDir:         pkiDir,
		keys:           keys,
		serverNames:    append([]string(nil), cfg.InternalGRPC.TLS.ServerNames...),
		serverValidity: time.Duration(cfg.InternalGRPC.TLS.ServerCertValidityHours) * time.Hour,
		clientCertTTL:  time.Duration(cfg.InternalGRPC.TLS.ClientCertValidityHours) * time.Hour,
		revocations:    revocations,
	}
	if authority.serverValidity <= 0 {
		authority.serverValidity = 30 * 24 * time.Hour
	}
	if authority.clientCertTTL <= 0 {
		authority.clientCertTTL = 24 * time.Hour
	}
	if err := authority.RefreshServerCertificate(ctx); err != nil {
		return nil, err
	}
	return authority, nil
}

func removeLegacyCAFiles(pkiDir string) {
	for _, name := range []string{legacyCACertFileName, legacyCAKeyFileName} {
		path := filepath.Join(pkiDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove legacy file-based CA", "path", path, "error", err)
		}
	}
}

func (a *TLSAuthority) HTTPConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: a.tlsConfigForClient,
		MinVersion:         tls.VersionTLS13,
	}
}

// tlsConfigForClient builds a per-handshake config with the current server
// leaf and the current trust bundle, so a rotation takes effect on new
// connections without a restart. Established sessions are unaffected.
func (a *TLSAuthority) tlsConfigForClient(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeKeyTimeout)
	defer cancel()
	mats, err := a.keys.Verifying(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return nil, fmt.Errorf("load verifying CAs: %w", err)
	}
	pool := x509.NewCertPool()
	for _, mat := range mats {
		if mat.Cert == nil {
			return nil, fmt.Errorf("signing key %s has no certificate", mat.Record.ID)
		}
		pool.AddCert(mat.Cert)
	}
	a.mu.RLock()
	leaf := a.serverCert
	a.mu.RUnlock()
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
		// The handshake uses this config wholesale, so it must carry the
		// ALPN protocols ServeTLS would otherwise negotiate: without h2
		// the gRPC clients cannot connect.
		NextProtos: []string{"h2", "http/1.1"},
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return nil
			}
			return a.revocations.Check(state.PeerCertificates[0])
		},
	}, nil
}

// RefreshServerCertificate re-issues this replica's server leaf when it no
// longer chains to a trusted CA or is near expiry. The leaf always chains
// to the oldest trusted CA: through a rotation that is the retiring CA, so
// agents holding either bundle verify it, and only after rotate-finish does
// the leaf flip to the new CA. Replicas call this at startup and on a
// ticker; rotation never requires a restart.
func (a *TLSAuthority) RefreshServerCertificate(ctx context.Context) error {
	mats, err := a.keys.Verifying(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return fmt.Errorf("load verifying CAs: %w", err)
	}
	if len(mats) == 0 || mats[0].Cert == nil || mats[0].Key == nil {
		return errors.New("no active internal CA")
	}
	// Oldest trusted CA signs the leaf: the retiring key while a rotation
	// overlaps, otherwise the active key.
	signer := mats[len(mats)-1]
	if signer.Cert == nil || signer.Key == nil {
		return fmt.Errorf("signing key %s has no CA material", signer.Record.ID)
	}
	pool := x509.NewCertPool()
	for _, mat := range mats {
		if mat.Cert != nil {
			pool.AddCert(mat.Cert)
		}
	}

	a.mu.RLock()
	leaf, cached := a.serverLeaf, a.serverCert
	a.mu.RUnlock()
	if leaf == nil {
		if loaded, loadedLeaf, err := loadKeyPair(a.serverCertPath(), a.serverKeyPath()); err == nil {
			leaf, cached = loadedLeaf, loaded
		}
	}
	if leaf != nil && serverLeafTrusted(leaf, pool) && time.Now().UTC().Before(serverLeafRenewAt(leaf, a.serverValidity)) {
		a.mu.Lock()
		a.serverCert, a.serverLeaf = cached, leaf
		a.mu.Unlock()
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}
	now := time.Now().UTC()
	serial, err := randomCertificateSerial()
	if err != nil {
		return fmt.Errorf("create server certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: firstServerName(a.serverNames),
		},
		DNSNames:              append([]string(nil), a.serverNames...),
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(a.serverValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, signer.Cert, key.Public(), signer.Key)
	if err != nil {
		return fmt.Errorf("create server cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal server key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(a.serverCertPath(), certPEM, 0o644); err != nil {
		return fmt.Errorf("write server cert: %w", err)
	}
	if err := os.WriteFile(a.serverKeyPath(), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write server key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load server keypair: %w", err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse server leaf: %w", err)
	}
	cert.Leaf = parsed
	a.mu.Lock()
	a.serverCert, a.serverLeaf = cert, parsed
	a.mu.Unlock()
	return nil
}

// serverLeafTrusted reports whether the leaf chains to any trusted CA.
func serverLeafTrusted(leaf *x509.Certificate, pool *x509.CertPool) bool {
	if leaf == nil {
		return false
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: time.Now().UTC(),
	})
	return err == nil
}

// serverLeafRenewAt is the moment a still-trusted leaf is re-issued anyway,
// one third of its validity before expiry.
func serverLeafRenewAt(leaf *x509.Certificate, validity time.Duration) time.Time {
	renewBefore := validity / 3
	if renewBefore <= 0 {
		renewBefore = time.Hour
	}
	return leaf.NotAfter.Add(-renewBefore)
}

func (a *TLSAuthority) serverCertPath() string { return filepath.Join(a.pkiDir, serverCertFileName) }
func (a *TLSAuthority) serverKeyPath() string  { return filepath.Join(a.pkiDir, serverKeyFileName) }

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

// TrustBundle returns the CA certificates agents and dashboards pin: the
// active CA first, then the retiring CA while a rotation overlaps.
func (a *TLSAuthority) TrustBundle(ctx context.Context) ([]byte, error) {
	return a.keys.PublicBundle(ctx, signkeys.ScopeInternalCA)
}

// ClusterIdentity is the hash of the active CA. It flips at rotate-start;
// VerifyClusterID accepts the retiring identity too, so pre-renewal agents
// stay connected through the overlap.
func (a *TLSAuthority) ClusterIdentity(ctx context.Context) (string, error) {
	active, err := a.keys.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return "", err
	}
	return clusterIdentityOfCA(active.Record.PublicPEM), nil
}

// VerifyClusterID reports whether id is a trusted cluster identity: the
// active CA, or the retiring CA while a rotation overlaps.
func (a *TLSAuthority) VerifyClusterID(ctx context.Context, id string) (bool, error) {
	mats, err := a.keys.Verifying(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return false, err
	}
	for _, mat := range mats {
		if clusterIdentityOfCA(mat.Record.PublicPEM) == id {
			return true, nil
		}
	}
	return false, nil
}

func clusterIdentityOfCA(caPEM []byte) string {
	digest := sha256.Sum256(bytes.TrimSpace(caPEM))
	return hex.EncodeToString(digest[:])
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

func (a *TLSAuthority) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
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
	return a.issueClientCertificate(ctx, CallerAgent, agentID, csr.PublicKey)
}

func (a *TLSAuthority) IssueManagedDashboardCertificate(ctx context.Context, id, csrPEM string) (*agentv1.EnrollResponse, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, status.Error(codes.FailedPrecondition, "managed dashboard caller id is not configured")
	}
	csr, err := parseClientCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	return a.issueClientCertificate(ctx, CallerDashboard, id, csr.PublicKey)
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

func (a *TLSAuthority) issueClientCertificate(ctx context.Context, class CallerClass, id string, publicKey any) (*agentv1.EnrollResponse, error) {
	active, err := a.keys.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "signing keys are unavailable")
	}
	if active.Cert == nil || active.Key == nil {
		return nil, status.Error(codes.Unavailable, "active internal CA has no material")
	}
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
	certDER, err := x509.CreateCertificate(rand.Reader, template, active.Cert, publicKey, active.Key)
	if err != nil {
		return nil, status.Error(codes.Internal, "issue client certificate")
	}
	bundle, err := a.keys.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "signing keys are unavailable")
	}
	return &agentv1.EnrollResponse{
		CertPem:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		CaPem:    string(bundle),
		NotAfter: timestamppb.New(notAfter),
	}, nil
}

type ClientIdentityMaterial struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

func (a *TLSAuthority) EnsureClientIdentity(ctx context.Context, class CallerClass, id string) (ClientIdentityMaterial, error) {
	if strings.TrimSpace(id) == "" {
		return ClientIdentityMaterial{}, errors.New("client identity id is required")
	}
	dir := filepath.Join(a.pkiDir, clientCertsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ClientIdentityMaterial{}, err
	}
	bundle, err := a.keys.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return ClientIdentityMaterial{}, errors.New("signing keys hold no CA certificates")
	}
	base := string(class) + "-" + strings.TrimSpace(id)
	certPath := filepath.Join(dir, base+".crt")
	keyPath := filepath.Join(dir, base+".key")
	if cert, leaf, err := loadKeyPair(certPath, keyPath); err == nil && time.Now().UTC().Before(leaf.NotAfter.Add(-time.Hour)) {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:       pool,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			CurrentTime: time.Now().UTC(),
		}); err == nil {
			certPEM, err := os.ReadFile(certPath)
			if err != nil {
				return ClientIdentityMaterial{}, err
			}
			keyPEM, err := os.ReadFile(keyPath)
			if err != nil {
				return ClientIdentityMaterial{}, err
			}
			_ = cert
			return ClientIdentityMaterial{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: append([]byte(nil), bundle...)}, nil
		}
		// The cached identity no longer chains to a trusted CA (its CA was
		// retired and finished); fall through and re-issue.
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("generate client key: %w", err)
	}
	active, err := a.keys.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	if active.Cert == nil || active.Key == nil {
		return ClientIdentityMaterial{}, errors.New("active internal CA has no material")
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
	certDER, err := x509.CreateCertificate(rand.Reader, template, active.Cert, key.Public(), active.Key)
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
		CAPEM:   append([]byte(nil), bundle...),
	}, nil
}

// IssueClientCertificate mints a fresh client identity from shared keys
// without node-local state. The operator CLI uses it; replicas serving
// dashboards and builders use the cached EnsureClientIdentity.
func IssueClientCertificate(ctx context.Context, keys signkeys.Provider, class CallerClass, id string, ttl time.Duration) (ClientIdentityMaterial, error) {
	switch class {
	case CallerAgent, CallerBuilder, CallerDashboard:
	default:
		return ClientIdentityMaterial{}, errors.New("unknown client certificate caller class")
	}
	if strings.TrimSpace(id) == "" {
		return ClientIdentityMaterial{}, errors.New("client identity id is required")
	}
	if keys == nil {
		return ClientIdentityMaterial{}, errors.New("client identity requires a signing-key provider")
	}
	if ttl <= 0 {
		return ClientIdentityMaterial{}, errors.New("client identity ttl must be greater than 0")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("generate client key: %w", err)
	}
	active, err := keys.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	if active.Cert == nil || active.Key == nil {
		return ClientIdentityMaterial{}, errors.New("active internal CA has no material")
	}
	now := time.Now().UTC()
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
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, active.Cert, key.Public(), active.Key)
	if err != nil {
		return ClientIdentityMaterial{}, fmt.Errorf("issue client certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := marshalPrivateKeyPEM(key)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	bundle, err := keys.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return ClientIdentityMaterial{}, err
	}
	return ClientIdentityMaterial{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: bundle}, nil
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

func (a *TLSAuthority) EnsureDashboardClientIdentity(ctx context.Context, id string) (ClientIdentityMaterial, error) {
	return a.EnsureClientIdentity(ctx, CallerDashboard, id)
}

func (a *TLSAuthority) EnsureBuilderClientIdentity(ctx context.Context, id string) (ClientIdentityMaterial, error) {
	return a.EnsureClientIdentity(ctx, CallerBuilder, id)
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
