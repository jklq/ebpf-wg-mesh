package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc"
)

const (
	managedDashboardCAFileName   = "controlplane-ca.pem"
	managedDashboardCertFileName = "controlplane-cert.pem"
	managedDashboardKeyFileName  = "controlplane-key.pem"
)

type dashboardCertificateIssuer interface {
	IssueManagedDashboardCertificate(context.Context, *agentv1.ManagedDashboardCertificateRequest, ...grpc.CallOption) (*agentv1.EnrollResponse, error)
}

func (a *App) ensureManagedDashboardIdentity(ctx context.Context, issuer dashboardCertificateIssuer, state *agentv1.DesiredNodeState) (bool, error) {
	if !hasManagedDashboardService(state) {
		return false, nil
	}
	secretsDir := filepath.Clean(a.cfg.Runtime.ManagedDashboardSecretsDir)
	if secretsDir == "." || !filepath.IsAbs(secretsDir) {
		return false, errors.New("managed dashboard secrets directory is not configured")
	}

	key, keyPEM, err := loadOrCreateManagedDashboardKey(secretsDir)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	leaf, err := loadManagedDashboardCertificate(secretsDir, keyPEM)
	existingValid := err == nil && now.Before(leaf.NotAfter)
	if existingValid && !certificateNeedsRenewal(leaf, now, time.Duration(a.cfg.ControlPlane.TLS.RenewBeforeMinutes)*time.Minute) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("replacing invalid managed dashboard certificate", "agent_id", a.cfg.Node.ID, "error", err)
	}

	issuedLeaf, err := a.renewManagedDashboardIdentity(ctx, issuer, secretsDir, key, keyPEM)
	if err != nil {
		if existingValid {
			slog.Warn("managed dashboard certificate renewal failed; using current certificate", "agent_id", a.cfg.Node.ID, "not_after", leaf.NotAfter, "error", err)
			return false, nil
		}
		return false, err
	}
	slog.Info("managed dashboard identity ready", "agent_id", a.cfg.Node.ID, "not_after", issuedLeaf.NotAfter)
	return true, nil
}

func (a *App) renewManagedDashboardIdentity(ctx context.Context, issuer dashboardCertificateIssuer, secretsDir string, key *ecdsa.PrivateKey, keyPEM []byte) (*x509.Certificate, error) {
	csrPEM, err := createCSR("managed-dashboard", key)
	if err != nil {
		return nil, err
	}
	resp, err := issuer.IssueManagedDashboardCertificate(ctx, &agentv1.ManagedDashboardCertificateRequest{
		AgentId: a.cfg.Node.ID,
		CsrPem:  string(csrPEM),
	})
	if err != nil {
		return nil, fmt.Errorf("issue certificate: %w", err)
	}
	certPEM := []byte(resp.GetCertPem())
	caPEM := []byte(resp.GetCaPem())
	issuedLeaf, err := validateManagedDashboardIdentity(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(secretsDir, managedDashboardCAFileName), caPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write managed dashboard ca: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(secretsDir, managedDashboardCertFileName), certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write managed dashboard certificate: %w", err)
	}
	return issuedLeaf, nil
}

func hasManagedDashboardService(state *agentv1.DesiredNodeState) bool {
	for _, svc := range state.GetServices() {
		if isManagedDashboardService(svc) {
			return true
		}
	}
	return false
}

func loadOrCreateManagedDashboardKey(dir string) (*ecdsa.PrivateKey, []byte, error) {
	keyPath := filepath.Join(dir, managedDashboardKeyFileName)
	if keyPEM, err := os.ReadFile(keyPath); err == nil {
		key, err := parseECPrivateKey(keyPEM)
		if err != nil {
			return nil, nil, fmt.Errorf("parse managed dashboard key: %w", err)
		}
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return nil, nil, fmt.Errorf("secure managed dashboard key: %w", err)
		}
		return key, keyPEM, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read managed dashboard key: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate managed dashboard key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal managed dashboard key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write managed dashboard key: %w", err)
	}
	return key, keyPEM, nil
}

func loadManagedDashboardCertificate(dir string, keyPEM []byte) (*x509.Certificate, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, managedDashboardCertFileName))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, managedDashboardCAFileName))
	if err != nil {
		return nil, err
	}
	return validateManagedDashboardIdentity(certPEM, keyPEM, caPEM)
}

func validateManagedDashboardIdentity(certPEM, keyPEM, caPEM []byte) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("validate managed dashboard keypair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("issued managed dashboard certificate is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse managed dashboard certificate: %w", err)
	}
	if len(leaf.Subject.OrganizationalUnit) == 0 || leaf.Subject.OrganizationalUnit[0] != "dashboard" {
		return nil, errors.New("issued certificate is not a dashboard identity")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("issued managed dashboard ca is invalid")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("verify managed dashboard certificate: %w", err)
	}
	return leaf, nil
}

func certificateNeedsRenewal(cert *x509.Certificate, now time.Time, configured time.Duration) bool {
	if cert == nil || !now.Before(cert.NotAfter) {
		return true
	}
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	if configured <= 0 || configured >= lifetime {
		configured = lifetime / 3
	}
	return !now.Before(cert.NotAfter.Add(-configured))
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
