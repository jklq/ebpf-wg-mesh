//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
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
		TLS: config.IngressTLSConfig{ACME: config.ACMEConfig{DirectoryURL: directory, CAFile: caFile}},
	}})

	bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{
		NodeID:       "envoy-tls-" + id,
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
		"--publish", fmt.Sprintf("127.0.0.1::%d", tlsE2EHTTPPort), "--publish", fmt.Sprintf("127.0.0.1::%d", tlsE2EHTTPSPort),
		"--volume", bootstrapPath+":/etc/envoy/envoy.yaml:ro",
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

	// A generated hostname without healthy backends still answers its challenge.
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

	roots := x509.NewCertPool()
	rootPEM := fetchBody(t, pebbleClient, "https://"+dockerHostPort(t, docker, pebbleName, "15000/tcp")+"/roots/0")
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
	if !slices.Contains(leaf.DNSNames, generated.GetHostname()) {
		t.Fatalf("served certificate names = %v, want %s", leaf.DNSNames, generated.GetHostname())
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
