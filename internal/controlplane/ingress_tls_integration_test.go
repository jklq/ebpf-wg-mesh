//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/certificates"
	"ebof-wg-mesh/internal/controlplane/ingressnodes"
	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/testutil"
)

const (
	pebbleImage       = "ghcr.io/letsencrypt/pebble:latest"
	challTestSrvImage = "ghcr.io/letsencrypt/pebble-challtestsrv:latest"
	tlsEnvoyImage     = "envoyproxy/envoy:v1.36-latest"
	tlsE2EHTTPPort    = 8080
	tlsE2EHTTPSPort   = 8443
)

// TestIngressServesAutomaticHTTPSEndToEnd runs the full certificate path with
// real parts: Pebble as the ACME CA, a DNS server that points every name at a
// real Envoy, and the control plane as Envoy's xDS authority. Pebble validates
// HTTP-01 through Envoy; Envoy then serves the issued certificate over SDS.
func TestIngressServesAutomaticHTTPSEndToEnd(t *testing.T) {
	requireDocker(t)
	docker := localteststack.ExecDockerRunner{}
	ctx := context.Background()
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	network := "ingress-tls-" + id
	dockerRun(t, docker, "network", "create", network)
	t.Cleanup(func() { _, _ = docker.Run(context.Background(), "network", "rm", network) })

	dnsName := "challtestsrv-" + id
	dockerContainer(t, docker, dnsName, "--network", network, "--publish", "127.0.0.1::8055", challTestSrvImage,
		"-defaultIPv6", "", "-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", "")

	stateDir := t.TempDir()
	pebbleConfig := fmt.Sprintf(`{"pebble": {
		"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
		"certificate": "test/certs/localhost/cert.pem", "privateKey": "test/certs/localhost/key.pem",
		"httpPort": %d, "tlsPort": 5001, "ocspResponderURL": "", "externalAccountBindingRequired": false}}`, tlsE2EHTTPPort)
	if err := os.WriteFile(filepath.Join(stateDir, "pebble.json"), []byte(pebbleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	pebbleName := "pebble-" + id
	dockerContainer(t, docker, pebbleName, "--network", network,
		"--publish", "127.0.0.1::14000", "--publish", "127.0.0.1::15000",
		"--env", "PEBBLE_VA_NOSLEEP=1", "--env", "PEBBLE_WFE_NONCEREJECT=0",
		"--volume", stateDir+":/config:ro",
		pebbleImage, "-config", "/config/pebble.json", "-dnsserver", dnsName+":8053")
	// Pebble's own directory TLS chains to this fixed test root.
	caFile := filepath.Join(stateDir, "pebble-minica.pem")
	dockerRun(t, docker, "cp", pebbleName+":/test/certs/pebble.minica.pem", caFile)
	minica, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pebbleClient := httpsClientTrusting(t, minica)
	directory := "https://" + dockerHostPort(t, docker, pebbleName, "14000/tcp") + "/dir"
	waitForHTTP(t, pebbleClient, directory)

	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello over %s", r.Header.Get("X-Forwarded-Proto"))
	}), ReadHeaderTimeout: 5 * time.Second}
	backendLn, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = backend.Serve(backendLn) }()
	t.Cleanup(func() { _ = backend.Close() })

	// The IPv4 host gateway: Docker Desktop may map host-gateway to IPv6, which the IPv4 listeners miss.
	hostGateway, err := localteststack.ResolveDockerHostGateway(ctx)
	if err != nil {
		t.Skipf("docker host gateway is unavailable: %v", err)
	}
	const staticHost = "app.tls.test"
	const suffix = "apps.tls.test"
	wildcard, wildcardRoot := testExternalWildcard(t, suffix)
	platformCertFile, platformKeyFile := filepath.Join(stateDir, "platform.crt"), filepath.Join(stateDir, "platform.key")
	if err := os.WriteFile(platformCertFile, wildcard.ChainPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platformKeyFile, wildcard.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	xdsPort := availableLocalPort(t)
	cp := startSystemControlPlane(t, systemControlPlaneOptions{ingress: &config.IngressConfig{
		PublicAddr:       suffix,
		XDSListen:        fmt.Sprintf("0.0.0.0:%d", xdsPort),
		HTTPListenAddrs:  []string{fmt.Sprintf(":%d", tlsE2EHTTPPort)},
		HTTPSListenAddrs: []string{fmt.Sprintf(":%d", tlsE2EHTTPSPort)},
		StaticRoutes: []config.StaticIngressRouteConfig{{
			Hosts:    []string{staticHost},
			Upstream: net.JoinHostPort(hostGateway, strconv.Itoa(backendLn.Addr().(*net.TCPAddr).Port)),
		}},
		TLS: config.IngressTLSConfig{ACME: config.ACMEConfig{DirectoryURL: directory, CAFile: caFile}, PlatformCertFile: platformCertFile, PlatformKeyFile: platformKeyFile},
	}})

	material, err := cp.server.ProvisionIngressIdentity(ctx, "envoy-tls-"+id)
	if err != nil {
		t.Fatal(err)
	}
	identityDir := filepath.Join(stateDir, "identity")
	if err := xds.WriteIdentity(identityDir, "/etc/envoy/identity", material); err != nil {
		t.Fatal(err)
	}
	registry := ingressnodes.New(cp.server.store.db)
	if err := registry.Register(ctx, "removed-envoy"); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{
		NodeID:      "envoy-tls-" + id,
		IdentityDir: "/etc/envoy/identity", ServerName: "localhost",
		XDSAddresses: []string{net.JoinHostPort(hostGateway, strconv.Itoa(xdsPort))},
		AdminAddress: "0.0.0.0:19000",
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPath := filepath.Join(stateDir, "envoy.yaml")
	if err := os.WriteFile(bootstrapPath, []byte(bootstrap), 0o644); err != nil {
		t.Fatal(err)
	}
	envoyName := "envoy-tls-" + id
	dockerContainer(t, docker, envoyName, "--network", network,
		"--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
		"--publish", fmt.Sprintf("127.0.0.1::%d", tlsE2EHTTPPort), "--publish", fmt.Sprintf("127.0.0.1::%d", tlsE2EHTTPSPort),
		"--volume", bootstrapPath+":/etc/envoy/envoy.yaml:ro",
		"--volume", identityDir+":/etc/envoy/identity:ro",
		tlsEnvoyImage, "envoy", "--config-path", "/etc/envoy/envoy.yaml")
	envoyIP := strings.TrimSpace(string(dockerRun(t, docker, "inspect", "--format",
		fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, network), envoyName)))
	// Every name the CA looks up now resolves to Envoy.
	dnsAPI := "http://" + dockerHostPort(t, docker, dnsName, "8055/tcp")
	resp, err := http.Post(dnsAPI+"/set-default-ipv4", "application/json", strings.NewReader(fmt.Sprintf(`{"ip":%q}`, envoyIP)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// An absent active instance blocks real HTTP-01 validation until the operator
	// confirms permanent removal. The live Envoy alone must then finish issuance.
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		challenges, err := cp.server.store.certificates.ListChallenges(ctx)
		if err != nil || len(challenges) == 0 {
			return false, err
		}
		nodes, converged, err := cp.server.ingress.Applied(ctx)
		return nodes == 2 && !converged, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Retire(ctx, "removed-envoy"); err != nil {
		t.Fatal(err)
	}

	// Generated hostnames share the operator-provisioned wildcard without ACME.
	service := domainOperationFixture(t, cp.server.store, "owner")
	domains := NewDomains(cp.server.store.platform(), noopNotifier{}, cp.server.ingress, cp.server.certificates, suffix, staticCNAMEResolver{})
	generated, err := domains.GenerateDomainBinding(ctx, testUser("owner"), &platformv1.GenerateDomainBindingRequest{ServiceId: service.ID, TargetPort: 8080})
	if err != nil {
		t.Fatal(err)
	}

	for _, hostname := range []string{staticHost, generated.GetHostname()} {
		err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Minute, Interval: time.Second}, func(ctx context.Context) (bool, error) {
			status := cp.server.certificates.Status(ctx, hostname)
			if status.State == certificates.StateFailed {
				return false, fmt.Errorf("certificate for %s failed: %s", hostname, status.Message)
			}
			return status.State == certificates.StateActive, nil
		})
		if err != nil {
			logs, _ := docker.Run(context.Background(), "logs", "--tail", "60", envoyName)
			t.Fatalf("wait for certificate %s: %v\nenvoy logs:\n%s", hostname, err, logs)
		}
	}
	binding, err := domains.GetDomainBinding(ctx, testUser("owner"), generated.GetHostname())
	if err != nil {
		t.Fatal(err)
	}
	if got := binding.GetCertificate(); got.GetState() != platformv1.DomainCertificateState_DOMAIN_CERTIFICATE_STATE_ACTIVE || got.GetExpiresAt() == nil {
		t.Fatalf("API certificate = %v, want active with expiry", got)
	}
	for i := range 5 {
		owner := fmt.Sprintf("owner-%d", i)
		added := domainOperationFixture(t, cp.server.store, owner)
		if _, err := domains.GenerateDomainBinding(ctx, testUser(owner), &platformv1.GenerateDomainBindingRequest{ServiceId: added.ID, TargetPort: 8080}); err != nil {
			t.Fatal(err)
		}
	}
	// Creating services adds routes but never per-host certificates or challenges.
	if err := cp.server.ingress.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	certificatesInDB, err := cp.server.store.certificates.ListCertificates(ctx)
	if err != nil || len(certificatesInDB) != 1 || certificatesInDB[0].Hostname != staticHost {
		t.Fatalf("certificate orders after adding services = %+v, %v", certificatesInDB, err)
	}

	roots := x509.NewCertPool()
	rootPEM := fetchBody(t, pebbleClient, "https://"+dockerHostPort(t, docker, pebbleName, "15000/tcp")+"/roots/0")
	roots.AppendCertsFromPEM(wildcardRoot)
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("Pebble root is not PEM: %s", rootPEM)
	}
	httpsAddr := dockerHostPort(t, docker, envoyName, fmt.Sprintf("%d/tcp", tlsE2EHTTPSPort))
	httpAddr := dockerHostPort(t, docker, envoyName, fmt.Sprintf("%d/tcp", tlsE2EHTTPPort))

	// Envoy serves the issued chain once the snapshot with the certificate lands.
	var body string
	err = testutil.Poll(ctx, testutil.PollConfig{Timeout: time.Minute, Interval: 500 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		got, err := getVia(ctx, httpsAddr, "https://"+staticHost+"/hello", roots)
		if err != nil {
			return false, nil
		}
		body = got
		return true, nil
	})
	if err != nil {
		t.Fatalf("HTTPS request through Envoy never succeeded")
	}
	if body != "hello over https" {
		t.Fatalf("HTTPS body = %q, want the backend to see X-Forwarded-Proto https", body)
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", httpsAddr, &tls.Config{
		ServerName: generated.GetHostname(), RootCAs: roots, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS handshake for the generated hostname: %v", err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	_ = conn.Close()
	if !slices.Contains(leaf.DNSNames, "*."+suffix) {
		t.Fatalf("served certificate names = %v, want shared wildcard %s", leaf.DNSNames, "*."+suffix)
	}

	// Filesystem SDS reloads the dedicated ingress credential. Revoking the old
	// leaf closes its ADS stream; the next stream must use the new TLS connection.
	renewedIdentity, err := cp.server.ProvisionIngressIdentity(ctx, "envoy-tls-"+id)
	if err != nil {
		t.Fatal(err)
	}
	if err := xds.WriteIdentity(identityDir, "/etc/envoy/identity", renewedIdentity); err != nil {
		t.Fatal(err)
	}
	oldBlock, _ := pem.Decode(material.CertPEM)
	oldIdentity, err := x509.ParseCertificate(oldBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.server.authority.Revocations().Add(oldIdentity.SerialNumber.Text(16)); err != nil {
		t.Fatal(err)
	}

	// External DNS-01 renewal changes the shared wildcard without restarting the
	// control plane or Envoy, and creates no individual orders.
	renewedWildcard, renewedRoot := testExternalWildcard(t, suffix)
	roots.AppendCertsFromPEM(renewedRoot)
	for path, data := range map[string][]byte{platformCertFile: renewedWildcard.ChainPEM, platformKeyFile: renewedWildcard.KeyPEM} {
		if err := os.WriteFile(path+".new", data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	if err := cp.server.ingress.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 250 * time.Millisecond}, func(context.Context) (bool, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", httpsAddr, &tls.Config{ServerName: generated.GetHostname(), RootCAs: roots, MinVersion: tls.VersionTLS12})
		if err != nil {
			return false, nil
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.Cmp(leaf.SerialNumber) != 0, nil
	}); err != nil {
		t.Fatalf("wildcard and ingress identity rotation did not reach Envoy: %v", err)
	}
	if count, err := cp.server.store.certificates.ListCertificates(ctx); err != nil || len(count) != 1 {
		t.Fatalf("renewal created certificate orders: %v, %v", count, err)
	}

	// Plain HTTP redirects to HTTPS, keeping the path.
	req, err := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/hello?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = staticHost
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirect, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = redirect.Body.Close()
	want := fmt.Sprintf("https://%s:%d/hello?x=1", staticHost, tlsE2EHTTPSPort)
	if redirect.StatusCode != http.StatusPermanentRedirect || redirect.Header.Get("Location") != want {
		t.Fatalf("HTTP response = %d %q, want 308 to %s", redirect.StatusCode, redirect.Header.Get("Location"), want)
	}
}

// The external DNS-01 provisioner is represented by an independent test CA;
// this material never passes through the platform's ACME issuer or certificate DB.
func testExternalWildcard(t *testing.T, suffix string) (certificates.Issued, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "external DNS-01 test CA"}, IsCA: true, BasicConstraintsValid: true, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(72 * time.Hour), KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: serial, DNSNames: []string{"*." + suffix}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), root...)
	return certificates.Issued{ChainPEM: chain, KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}, root
}

func dockerRun(t *testing.T, docker localteststack.DockerRunner, args ...string) []byte {
	t.Helper()
	out, err := docker.Run(context.Background(), args...)
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func dockerContainer(t *testing.T, docker localteststack.DockerRunner, name string, args ...string) {
	t.Helper()
	dockerRun(t, docker, append([]string{"run", "--detach", "--name", name}, args...)...)
	t.Cleanup(func() { _, _ = docker.Run(context.Background(), "rm", "--force", name) })
}

func dockerHostPort(t *testing.T, docker localteststack.DockerRunner, container, port string) string {
	t.Helper()
	out := strings.TrimSpace(string(dockerRun(t, docker, "port", container, port)))
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first)
}

func httpsClientTrusting(t *testing.T, rootPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		t.Fatal("root is not PEM")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

func waitForHTTP(t *testing.T, client *http.Client, url string) {
	t.Helper()
	err := testutil.Poll(context.Background(), testutil.PollConfig{Timeout: 30 * time.Second, Interval: 250 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		resp, err := client.Get(url)
		if err != nil {
			return false, nil
		}
		_ = resp.Body.Close()
		return resp.StatusCode < 300, nil
	})
	if err != nil {
		t.Fatalf("wait for %s: %v", url, err)
	}
}

func fetchBody(t *testing.T, client *http.Client, url string) []byte {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(body)
}

// getVia sends an HTTPS request for url to a fixed address, as a client whose
// DNS points the hostname at Envoy would.
func getVia(ctx context.Context, addr, url string, roots *x509.CertPool) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return string(body), nil
}
