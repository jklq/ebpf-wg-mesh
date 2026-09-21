package agent

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
	"os"
	"path/filepath"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc/credentials"
)

const (
	agentTLSDirName   = "tls"
	agentKeyFileName  = "client.key"
	agentCertFileName = "client.crt"
	agentCAFileName   = "ca.crt"
)

type clientTLSMaterial struct {
	certificate     tls.Certificate
	rootCAs         *x509.CertPool
	notAfter        time.Time
	clusterIdentity string
}

func (a *App) clientCredentials(ctx context.Context) (credentials.TransportCredentials, time.Time, string, error) {
	slog.Info("ensuring client tls material", "agent_id", a.cfg.Node.ID)
	material, err := a.ensureClientTLSMaterial(ctx)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	slog.Info("client tls material ready", "agent_id", a.cfg.Node.ID, "not_after", material.notAfter)
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{material.certificate},
		RootCAs:      material.rootCAs,
		ServerName:   a.cfg.ControlPlane.TLS.ServerName,
		MinVersion:   tls.VersionTLS13,
	}), material.notAfter, material.clusterIdentity, nil
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
	slog.Info("enrolling client certificate", "agent_id", a.cfg.Node.ID, "renewal", current != nil)
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
	req := &agentv1.EnrollRequest{
		AgentId: a.cfg.Node.ID,
		CsrPem:  string(csrPEM),
	}
	if current == nil {
		req.BootstrapToken = a.cfg.ControlPlane.TLS.BootstrapToken
	}
	addresses, err := a.controlPlaneCandidates()
	if err != nil {
		return nil, fmt.Errorf("load control-plane discovery set for enroll: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("control-plane discovery set is empty")
	}
	var failures []error
	for _, address := range addresses {
		conn, err := dialControlPlane(ctx, address, creds)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			err = a.replicaAttemptError(address, err)
			if errors.Is(err, errRedirectOwner) {
				return nil, err
			}
			if !shouldWalkNextReplica(err) {
				return nil, fmt.Errorf("enroll client certificate: %w", err)
			}
			failures = append(failures, err)
			continue
		}
		client := agentv1.NewAgentControlClient(conn)
		rpcCtx, cancel := replicaRPCContext(ctx)
		resp, err := client.Enroll(rpcCtx, req)
		cancel()
		_ = conn.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			err = a.replicaAttemptError(address, err)
			if errors.Is(err, errRedirectOwner) {
				return nil, err
			}
			if !shouldWalkNextReplica(err) {
				return nil, fmt.Errorf("enroll client certificate: %w", err)
			}
			failures = append(failures, err)
			continue
		}
		if resp == nil {
			return nil, errors.New("enroll client certificate: empty response")
		}
		if a.stateStore != nil {
			// The server returns the CA bundle (active first, then the
			// retiring CA through a rotation); the pinned identity is the
			// active CA. Initial enrollment fails closed on mismatch;
			// renewal runs over a channel authenticated by the pinned
			// roots, so it adopts the rotated identity.
			incoming, err := activeCAIdentity([]byte(resp.GetCaPem()))
			if err != nil {
				return nil, err
			}
			if current == nil {
				if err := a.stateStore.requireClusterIdentity(incoming); err != nil {
					return nil, err
				}
			}
			if err := a.stateStore.adoptClusterIdentity(incoming); err != nil {
				return nil, err
			}
			if err := a.stateStore.setReplicaAddresses(resp.GetReplicaAddresses()); err != nil {
				return nil, fmt.Errorf("persist control-plane replica addresses: %w", err)
			}
		}
		a.controlPlaneAddr = address
		slog.Info("client certificate enrolled", "agent_id", a.cfg.Node.ID, "address", address)
		if err := a.persistClientTLSMaterial(keyPEM, []byte(resp.GetCertPem()), []byte(resp.GetCaPem())); err != nil {
			return nil, err
		}
		return a.loadClientTLSMaterial()
	}
	return nil, fmt.Errorf("no reachable control-plane replica for enrollment: %w", errors.Join(failures...))
}

func (a *App) enrollmentCredentials(current *clientTLSMaterial) (credentials.TransportCredentials, error) {
	if current != nil {
		// Enrolled roots are always fresher than the bootstrap file: every
		// renewal persists the server's current bundle, so renewal after a
		// CA rotation verifies against roots that include the new CA. The
		// bootstrap file is only for the first enrollment.
		config := &tls.Config{
			RootCAs:    current.rootCAs,
			ServerName: a.cfg.ControlPlane.TLS.ServerName,
			MinVersion: tls.VersionTLS13,
		}
		if time.Now().UTC().Before(current.notAfter) {
			config.Certificates = []tls.Certificate{current.certificate}
		}
		return credentials.NewTLS(config), nil
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
		certificate:     cert,
		rootCAs:         pool,
		notAfter:        leaf.NotAfter,
		clusterIdentity: a.pinnedClusterIdentity(caPEM),
	}, nil
}

// pinnedClusterIdentity is the cluster identity hello sends: the value
// adopted at enrollment, which survives CA rotations that replace the
// bundle on disk. Without a state store (tests), it derives from the
// bundle's active CA.
func (a *App) pinnedClusterIdentity(caPEM []byte) string {
	if a.stateStore != nil {
		if id := a.stateStore.clusterIdentity(); id != "" {
			return id
		}
	}
	id, err := activeCAIdentity(caPEM)
	if err != nil {
		return ""
	}
	return id
}

// activeCAIdentity hashes the bundle's first certificate — the active CA —
// exactly as the control plane hashes it, so enrollment converges on the
// same identity on both sides through a rotation.
func activeCAIdentity(bundle []byte) (string, error) {
	trimmed := bytes.TrimSpace(bundle)
	if len(trimmed) == 0 {
		return "", errors.New("enrolled CA bundle is empty")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("enrolled CA bundle holds no certificate")
	}
	// The first block's bytes, hashed exactly as the server hashes the
	// active CA it encoded first.
	first := bytes.TrimSpace(trimmed[:len(trimmed)-len(rest)])
	digest := sha256.Sum256(first)
	return hex.EncodeToString(digest[:]), nil
}

func (a *App) loadOrCreateClientKey() (*ecdsa.PrivateKey, []byte, error) {
	keyPath := filepath.Join(a.clientTLSDir(), agentKeyFileName)
	if keyPEM, err := os.ReadFile(keyPath); err == nil {
		key, err := parseECPrivateKey(keyPEM)
		if err != nil {
			return nil, nil, err
		}
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return nil, nil, fmt.Errorf("secure client key: %w", err)
		}
		return key, keyPEM, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read client key: %w", err)
	}
	if err := os.MkdirAll(a.clientTLSDir(), 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir client tls dir: %w", err)
	}
	if err := os.Chmod(a.clientTLSDir(), 0o700); err != nil {
		return nil, nil, fmt.Errorf("secure client tls dir: %w", err)
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
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write client key: %w", err)
	}
	return key, keyPEM, nil
}

func (a *App) persistClientTLSMaterial(keyPEM, certPEM, caPEM []byte) error {
	if err := os.MkdirAll(a.clientTLSDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir client tls dir: %w", err)
	}
	if err := os.Chmod(a.clientTLSDir(), 0o700); err != nil {
		return fmt.Errorf("secure client tls dir: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(a.clientTLSDir(), agentKeyFileName), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write client key: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(a.clientTLSDir(), agentCertFileName), certPEM, 0o644); err != nil {
		return fmt.Errorf("write client cert: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(a.clientTLSDir(), agentCAFileName), caPEM, 0o644); err != nil {
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
