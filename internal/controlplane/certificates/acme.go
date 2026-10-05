package certificates

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/config"

	"golang.org/x/crypto/acme"
)

// AccountStore keeps the ACME account key of each directory.
type AccountStore interface {
	LoadACMEAccount(ctx context.Context, directoryURL string) (Account, bool, error)
	SaveACMEAccount(ctx context.Context, account Account) error
}

// ACMEIssuer obtains certificates over ACME (RFC 8555) with HTTP-01. One
// account per directory is registered on first use and reused after.
type ACMEIssuer struct {
	cfg      config.ACMEConfig
	accounts AccountStore
	http     *http.Client

	mu     sync.Mutex
	client *acme.Client
	// stale marks a stored account that the CA no longer knows. The next
	// issuance registers a new account and replaces the stored one.
	stale bool
}

func NewACMEIssuer(cfg config.ACMEConfig, accounts AccountStore) (*ACMEIssuer, error) {
	if strings.TrimSpace(cfg.DirectoryURL) == "" {
		return nil, errors.New("ACME directory URL is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile := strings.TrimSpace(cfg.CAFile); caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ACME CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ACME CA file %s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &ACMEIssuer{
		cfg:      cfg,
		accounts: accounts,
		http:     &http.Client{Transport: transport, Timeout: time.Minute},
	}, nil
}

func (i *ACMEIssuer) Issue(ctx context.Context, hostname string, solver Solver) (Issued, error) {
	client, err := i.account(ctx)
	if err != nil {
		return Issued{}, err
	}
	issued, err := i.issue(ctx, client, hostname, solver)
	var acmeErr *acme.Error
	if errors.As(err, &acmeErr) && acmeErr.ProblemType == "urn:ietf:params:acme:error:accountDoesNotExist" {
		i.mu.Lock()
		i.client, i.stale = nil, true
		i.mu.Unlock()
	}
	return issued, err
}

func (i *ACMEIssuer) issue(ctx context.Context, client *acme.Client, hostname string, solver Solver) (Issued, error) {
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(hostname))
	if err != nil {
		return Issued{}, fmt.Errorf("create order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := i.authorize(ctx, client, hostname, authzURL, solver); err != nil {
			return Issued{}, err
		}
	}
	orderURI := order.URI
	order, err = client.WaitOrder(ctx, orderURI)
	if err != nil {
		return Issued{}, fmt.Errorf("wait for order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Issued{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{hostname}}, key)
	if err != nil {
		return Issued{}, fmt.Errorf("create certificate request: %w", err)
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		chain, err = finalizedChain(ctx, client, orderURI, err)
		if err != nil {
			return Issued{}, fmt.Errorf("finalize order: %w", err)
		}
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Issued{}, err
	}
	var chainPEM []byte
	for _, der := range chain {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return Issued{
		ChainPEM: chainPEM,
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// finalizedChain recovers the certificate of an order that the CA finalized
// although CreateOrderCert failed. CreateOrderCert follows the Location header
// of the finalize response, which RFC 8555 does not require and some CAs omit.
func finalizedChain(ctx context.Context, client *acme.Client, orderURI string, finalizeErr error) ([][]byte, error) {
	order, err := client.GetOrder(ctx, orderURI)
	if err != nil || (order.Status != acme.StatusProcessing && order.Status != acme.StatusValid) {
		return nil, finalizeErr
	}
	if order.Status != acme.StatusValid {
		if order, err = client.WaitOrder(ctx, orderURI); err != nil {
			return nil, err
		}
	}
	if order.CertURL == "" {
		return nil, finalizeErr
	}
	return client.FetchCert(ctx, order.CertURL, true)
}

func (i *ACMEIssuer) authorize(ctx context.Context, client *acme.Client, hostname, authzURL string, solver Solver) error {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("get authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, candidate := range authz.Challenges {
		if candidate.Type == "http-01" {
			challenge = candidate
			break
		}
	}
	if challenge == nil {
		return errors.New("the CA offers no http-01 challenge")
	}
	keyAuthorization, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}
	if err := solver.Present(ctx, hostname, challenge.Token, keyAuthorization); err != nil {
		return err
	}
	defer func() {
		// A leftover challenge expires on its own and its route is harmless.
		if err := solver.CleanUp(ctx, hostname, challenge.Token); err != nil {
			slog.Warn("remove ACME challenge", "hostname", hostname, "error", err)
		}
	}()
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("validate %s: %w", hostname, err)
	}
	return nil
}

// account returns a client bound to the registered account. A registration
// that fails is retried on the next issuance.
func (i *ACMEIssuer) account(ctx context.Context) (*acme.Client, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.client != nil {
		return i.client, nil
	}
	stored, ok, err := i.accounts.LoadACMEAccount(ctx, i.cfg.DirectoryURL)
	if err != nil {
		return nil, fmt.Errorf("load ACME account: %w", err)
	}
	if ok && !i.stale {
		key, err := parseAccountKey(stored.KeyPEM)
		if err != nil {
			return nil, err
		}
		i.client = i.newClient(key)
		i.client.KID = acme.KeyID(stored.URI)
		return i.client, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	client := i.newClient(key)
	account := &acme.Account{}
	if email := strings.TrimSpace(i.cfg.Email); email != "" {
		account.Contact = []string{"mailto:" + email}
	}
	if i.cfg.EABKeyID != "" {
		hmacKey, err := base64.RawURLEncoding.DecodeString(i.cfg.EABHMACKey)
		if err != nil {
			return nil, fmt.Errorf("decode EAB HMAC key: %w", err)
		}
		account.ExternalAccountBinding = &acme.ExternalAccountBinding{KID: i.cfg.EABKeyID, Key: hmacKey}
	}
	registered, err := client.Register(ctx, account, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		registered, err = client.GetReg(ctx, "")
	}
	if err != nil {
		return nil, fmt.Errorf("register ACME account: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := i.accounts.SaveACMEAccount(ctx, Account{
		DirectoryURL: i.cfg.DirectoryURL,
		URI:          registered.URI,
		KeyPEM:       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}); err != nil {
		return nil, fmt.Errorf("store ACME account: %w", err)
	}
	client.KID = acme.KeyID(registered.URI)
	i.client, i.stale = client, false
	return client, nil
}

func (i *ACMEIssuer) newClient(key crypto.Signer) *acme.Client {
	return &acme.Client{
		Key:          key,
		DirectoryURL: i.cfg.DirectoryURL,
		HTTPClient:   i.http,
		UserAgent:    "ebpf-wg-mesh-controlplane",
	}
}

func parseAccountKey(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("ACME account key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ACME account key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("ACME account key cannot sign")
	}
	return signer, nil
}
