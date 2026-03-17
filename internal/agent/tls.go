package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	agentTLSDirName   = "tls"
	agentKeyFileName  = "client.key"
	agentCertFileName = "client.crt"
	agentCAFileName   = "ca.crt"
)

type clientTLSMaterial struct {
	certificate tls.Certificate
	rootCAs     *x509.CertPool
	notAfter    time.Time
}

func (a *App) clientCredentials(ctx context.Context) (credentials.TransportCredentials, time.Time, error) {
	material, err := a.ensureClientTLSMaterial(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{material.certificate},
		RootCAs:      material.rootCAs,
		ServerName:   a.cfg.ControlPlane.TLS.ServerName,
		MinVersion:   tls.VersionTLS13,
	}), material.notAfter, nil
}

func (a *App) ensureClientTLSMaterial(ctx context.Context) (*clientTLSMaterial, error) {
	material, err := a.loadClientTLSMaterial()
	switch {
	case err == nil:
		renewBefore := time.Duration(a.cfg.ControlPlane.TLS.RenewBeforeMinutes) * time.Minute
		if time.Until(material.notAfter) > renewBefore {
			return material, nil
		}
		if renewed, renewErr := a.enrollClientCertificate(ctx, material); renewErr == nil {
			return renewed, nil
		} else if time.Now().UTC().Before(material.notAfter) {
			slog.Warn("client certificate renewal failed; using current certificate", "agent_id", a.cfg.Node.ID, "error", renewErr)
			return material, nil
		} else {
			return nil, renewErr
		}
	case errors.Is(err, os.ErrNotExist):
		return a.enrollClientCertificate(ctx, nil)
	default:
		return nil, err
	}
}

func (a *App) enrollClientCertificate(ctx context.Context, current *clientTLSMaterial) (*clientTLSMaterial, error) {
	key, keyPEM, err := a.loadOrCreateClientKey()
	if err != nil {
		return nil, err
	}
	csrPEM, err := createCSR(a.cfg.Node.ID, key)
	if err != nil {
		return nil, err
	}
	creds, err := a.enrollmentCredentials(current)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(a.cfg.ControlPlane.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial control plane enroll: %w", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentControlClient(conn)
	req := &agentv1.EnrollRequest{
		AgentId: a.cfg.Node.ID,
		CsrPem:  string(csrPEM),
	}
	if current == nil {
		req.BootstrapToken = a.cfg.ControlPlane.TLS.BootstrapToken
	}
	resp, err := client.Enroll(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("enroll client certificate: %w", err)
	}
	if err := a.persistClientTLSMaterial(keyPEM, []byte(resp.GetCertPem()), []byte(resp.GetCaPem())); err != nil {
		return nil, err
	}
	return a.loadClientTLSMaterial()
}

func (a *App) enrollmentCredentials(current *clientTLSMaterial) (credentials.TransportCredentials, error) {
	if current != nil && time.Now().UTC().Before(current.notAfter) {
		return credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{current.certificate},
			RootCAs:      current.rootCAs,
			ServerName:   a.cfg.ControlPlane.TLS.ServerName,
			MinVersion:   tls.VersionTLS13,
		}), nil
	}
	bootstrapCA, err := os.ReadFile(a.cfg.ControlPlane.TLS.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bootstrapCA) {
		return nil, errors.New("append bootstrap ca")
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: a.cfg.ControlPlane.TLS.ServerName,
		MinVersion: tls.VersionTLS13,
	}), nil
}

func (a *App) loadClientTLSMaterial() (*clientTLSMaterial, error) {
	certPath := filepath.Join(a.clientTLSDir(), agentCertFileName)
	keyPath := filepath.Join(a.clientTLSDir(), agentKeyFileName)
	caPath := filepath.Join(a.clientTLSDir(), agentCAFileName)

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("client certificate missing leaf")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse client certificate: %w", err)
	}
	cert.Leaf = leaf

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append enrolled ca")
	}
	return &clientTLSMaterial{
		certificate: cert,
		rootCAs:     pool,
		notAfter:    leaf.NotAfter,
	}, nil
}

func (a *App) loadOrCreateClientKey() (*ecdsa.PrivateKey, []byte, error) {
	keyPath := filepath.Join(a.clientTLSDir(), agentKeyFileName)
	if keyPEM, err := os.ReadFile(keyPath); err == nil {
		key, err := parseECPrivateKey(keyPEM)
		if err != nil {
			return nil, nil, err
		}
		return key, keyPEM, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read client key: %w", err)
	}
	if err := os.MkdirAll(a.clientTLSDir(), 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir client tls dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate client key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal client key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write client key: %w", err)
	}
	return key, keyPEM, nil
}

func (a *App) persistClientTLSMaterial(keyPEM, certPEM, caPEM []byte) error {
	if err := os.MkdirAll(a.clientTLSDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir client tls dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(a.clientTLSDir(), agentKeyFileName), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write client key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(a.clientTLSDir(), agentCertFileName), certPEM, 0o644); err != nil {
		return fmt.Errorf("write client cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(a.clientTLSDir(), agentCAFileName), caPEM, 0o644); err != nil {
		return fmt.Errorf("write client ca: %w", err)
	}
	return nil
}

func (a *App) clientTLSDir() string {
	return filepath.Join(a.cfg.Runtime.DataDir, agentTLSDirName)
}

func createCSR(agentID string, key *ecdsa.PrivateKey) ([]byte, error) {
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: strings.TrimSpace(agentID)},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create csr: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), nil
}

func parseECPrivateKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("decode client key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse client key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("client key is not ecdsa")
	}
	return key, nil
}
