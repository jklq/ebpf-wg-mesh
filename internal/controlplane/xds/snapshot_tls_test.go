package xds

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

type mapKeys map[string]KeyPair

func (m mapKeys) KeyPair(_ context.Context, fingerprint string) (KeyPair, error) {
	pair, ok := m[fingerprint]
	if !ok {
		return KeyPair{}, errors.New("unknown fingerprint")
	}
	return pair, nil
}

var testKeys = mapKeys{
	"aaaa": {CertificatePEM: []byte("CHAIN-A"), PrivateKeyPEM: []byte("PRIVATE-KEY-A")},
	"bbbb": {CertificatePEM: []byte("CHAIN-WILDCARD"), PrivateKeyPEM: []byte("PRIVATE-KEY-WILDCARD")},
}

func tlsInput() BuildInput {
	return BuildInput{
		Backends: []Backend{
			{Domain: "a.example.com", Upstream: "10.0.0.10:8080"},
			{Domain: "plain.example.com", Upstream: "10.0.0.11:8080"},
			{Domain: "gen.apps.example.net", Upstream: "10.0.0.12:8080"},
		},
		HTTPListenAddrs:  []string{":8080"},
		HTTPSListenAddrs: []string{":8443"},
		Certificates: []Certificate{
			{Name: "a.example.com", ServerNames: []string{"a.example.com"}, Fingerprint: "aaaa"},
			{Name: "*.apps.example.net", ServerNames: []string{"*.apps.example.net"}, Fingerprint: "bbbb"},
		},
		Keys: testKeys,
	}
}

func vhosts(t *testing.T, snap *Snapshot, config string) map[string]*routev3.VirtualHost {
	t.Helper()
	out := map[string]*routev3.VirtualHost{}
	for _, vhost := range routeResources(t, snap)[config].GetVirtualHosts() {
		out[vhost.GetName()] = vhost
	}
	return out
}

func TestBuildTerminatesTLSForCertifiedHostnames(t *testing.T) {
	t.Parallel()

	snap := mustBuild(t, tlsInput())
	listeners := listenerResources(t, snap)
	https, ok := listeners["ingress-https-8443"]
	if !ok {
		t.Fatalf("missing HTTPS listener, have %v", keys(listeners))
	}
	if filters := https.GetListenerFilters(); len(filters) != 1 || filters[0].GetName() != "envoy.filters.listener.tls_inspector" {
		t.Fatalf("listener filters = %v, want the TLS inspector", filters)
	}
	chains := map[string]*listenerv3.FilterChain{}
	for _, chain := range https.GetFilterChains() {
		chains[chain.GetName()] = chain
	}
	chain, ok := chains["tls/a.example.com"]
	if !ok {
		t.Fatalf("missing filter chain, have %v", keys(chains))
	}
	if got := chain.GetFilterChainMatch().GetServerNames(); !slices.Equal(got, []string{"a.example.com"}) {
		t.Fatalf("server names = %v", got)
	}
	var downstream tlsv3.DownstreamTlsContext
	if err := chain.GetTransportSocket().GetTypedConfig().UnmarshalTo(&downstream); err != nil {
		t.Fatal(err)
	}
	sds := downstream.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()
	if len(sds) != 1 || sds[0].GetName() != "tls/a.example.com" || sds[0].GetSdsConfig().GetAds() == nil {
		t.Fatalf("SDS config = %v, want tls/a.example.com over ADS", sds)
	}
	if got := downstream.GetCommonTlsContext().GetTlsParams().GetTlsMinimumProtocolVersion(); got != tlsv3.TlsParameters_TLSv1_2 {
		t.Fatalf("minimum TLS version = %v", got)
	}

	secrets := itemsFor(t, snap, resourcev3.SecretType)
	secret, ok := secrets["tls/a.example.com"].Resource.(*tlsv3.Secret)
	if !ok {
		t.Fatalf("missing secret, have %v", keys(secrets))
	}
	if got := secret.GetTlsCertificate().GetPrivateKey().GetInlineBytes(); string(got) != "PRIVATE-KEY-A" {
		t.Fatalf("secret key = %q", got)
	}
	if snap.Counts.Certificates != 2 {
		t.Fatalf("certificate count = %d, want 2", snap.Counts.Certificates)
	}
	if !slices.Contains(snap.RequiredTypes(), resourcev3.SecretType) {
		t.Fatalf("RequiredTypes = %v, want SDS while secrets exist", snap.RequiredTypes())
	}

	httpHosts := vhosts(t, snap, HTTPRouteConfigName)
	redirect := httpHosts["vhost/a.example.com/redirect"].GetRoutes()
	if len(redirect) != 1 || !redirect[0].GetRedirect().GetHttpsRedirect() || redirect[0].GetRedirect().GetPortRedirect() != 8443 {
		t.Fatalf("certified host HTTP routes = %v, want a redirect to :8443", redirect)
	}
	if got := redirect[0].GetRedirect().GetResponseCode(); got != routev3.RedirectAction_PERMANENT_REDIRECT {
		t.Fatalf("redirect code = %v, want 308", got)
	}
	if routes := httpHosts["vhost/plain.example.com"].GetRoutes(); len(routes) != 1 || routes[0].GetRoute() == nil {
		t.Fatalf("uncertified host HTTP routes = %v, want the backend", routes)
	}
	if _, ok := httpHosts["vhost/gen.apps.example.net/redirect"]; !ok {
		t.Fatalf("wildcard-covered host must redirect, have %v", keys(httpHosts))
	}

	httpsHosts := vhosts(t, snap, HTTPSRouteConfigName)
	if _, ok := httpsHosts["vhost/plain.example.com"]; ok {
		t.Fatal("uncertified host must not be routed over HTTPS")
	}
	secured := httpsHosts["vhost/a.example.com"]
	if secured == nil || !slices.Contains(secured.GetDomains(), "a.example.com:8443") {
		t.Fatalf("HTTPS vhost = %v, want host and host:port forms", secured)
	}
}

func TestBuildWithoutCertificatesHasNoHTTPSListener(t *testing.T) {
	t.Parallel()

	input := tlsInput()
	input.Certificates = nil
	snap := mustBuild(t, input)
	if _, ok := listenerResources(t, snap)["ingress-https-8443"]; ok {
		t.Fatal("an HTTPS listener without certificates has no filter chain")
	}
	if _, ok := routeResources(t, snap)[HTTPSRouteConfigName]; ok {
		t.Fatal("HTTPS route config must not exist without an HTTPS listener")
	}
	if routes := vhosts(t, snap, HTTPRouteConfigName)["vhost/a.example.com"].GetRoutes(); len(routes) != 1 || routes[0].GetRoute() == nil {
		t.Fatalf("routes = %v, want plaintext backend routes", routes)
	}
}

func TestBuildAnswersACMEChallenges(t *testing.T) {
	t.Parallel()

	input := tlsInput()
	input.Challenges = []Challenge{
		{Hostname: "a.example.com", Token: "tok-a", KeyAuthorization: "tok-a.thumb"},
		{Hostname: "new.example.com", Token: "tok-new", KeyAuthorization: "tok-new.thumb"},
		{Hostname: "evil.example.com", Token: "../../etc", KeyAuthorization: "x.y"},
	}
	snap := mustBuild(t, input)
	httpHosts := vhosts(t, snap, HTTPRouteConfigName)

	// Renewal of a certified host: the challenge route precedes the HTTPS redirect.
	routes := httpHosts["vhost/a.example.com/redirect"].GetRoutes()
	if len(routes) != 2 || routes[0].GetMatch().GetPath() != "/.well-known/acme-challenge/tok-a" || routes[1].GetRedirect() == nil {
		t.Fatalf("routes = %v, want challenge then redirect", routes)
	}
	if body := routes[0].GetDirectResponse().GetBody().GetInlineString(); body != "tok-a.thumb" {
		t.Fatalf("challenge body = %q", body)
	}

	// A hostname without backends still answers its first challenge.
	pending := httpHosts["acme/new.example.com"]
	if pending == nil || len(pending.GetRoutes()) != 1 || pending.GetRoutes()[0].GetDirectResponse().GetStatus() != 200 {
		t.Fatalf("pending vhost = %v", pending)
	}
	if _, ok := httpHosts["acme/evil.example.com"]; ok {
		t.Fatal("a token outside the base64url alphabet must be dropped")
	}
	if snap.Counts.Challenges != 2 {
		t.Fatalf("challenge count = %d, want 2", snap.Counts.Challenges)
	}
}

func TestBuildInputsHoldNoKeyMaterial(t *testing.T) {
	t.Parallel()

	snap := mustBuild(t, tlsInput())
	if bytes.Contains(snap.Inputs, []byte("PRIVATE-KEY")) || bytes.Contains(snap.Inputs, []byte("CHAIN-")) {
		t.Fatalf("published inputs leak key material: %s", snap.Inputs)
	}
	rebuilt, err := BuildFromInputs(context.Background(), snap.Inputs, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Version != snap.Version || !bytes.Equal(snapshotBytes(t, rebuilt), snapshotBytes(t, snap)) {
		t.Fatal("a replica must rebuild the identical snapshot from inputs and key pairs")
	}
	if _, err := BuildFromInputs(context.Background(), snap.Inputs, mapKeys{}); err == nil {
		t.Fatal("a replica without the key pair must fail closed")
	}

	renewed := tlsInput()
	renewed.Certificates[0].Fingerprint = "cccc"
	renewed.Keys = mapKeys{"cccc": testKeys["aaaa"], "bbbb": testKeys["bbbb"]}
	if mustBuild(t, renewed).Version == snap.Version {
		t.Fatal("a renewal must publish a new version")
	}
}

func TestBuildSkipsCertificatesWithoutKeyPair(t *testing.T) {
	t.Parallel()

	input := tlsInput()
	input.Keys = mapKeys{"bbbb": testKeys["bbbb"]}
	snap := mustBuild(t, input)
	if _, ok := itemsFor(t, snap, resourcev3.SecretType)["tls/a.example.com"]; ok {
		t.Fatal("a certificate without a key pair must be skipped")
	}
	if routes := vhosts(t, snap, HTTPRouteConfigName)["vhost/a.example.com"].GetRoutes(); len(routes) != 1 || routes[0].GetRoute() == nil {
		t.Fatalf("routes = %v, want plaintext routing", routes)
	}
}

func TestCanonicalCertificatesClaimEachServerNameOnce(t *testing.T) {
	t.Parallel()

	got := canonicalCertificates([]Certificate{
		{Name: "b", ServerNames: []string{"x.example.com"}, Fingerprint: "2"},
		{Name: "a", ServerNames: []string{"X.example.com", "y.example.com"}, Fingerprint: "1"},
		{Name: "c", ServerNames: []string{"y.example.com"}, Fingerprint: "3"},
		{Name: "", ServerNames: []string{"z.example.com"}, Fingerprint: "4"},
	})
	if len(got) != 1 || got[0].Name != "a" || !slices.Equal(got[0].ServerNames, []string{"x.example.com", "y.example.com"}) {
		t.Fatalf("canonical certificates = %+v", got)
	}
}

func TestWildcardCoversOneLabel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		serverName, host string
		want             bool
	}{
		{"*.apps.example.net", "x.apps.example.net", true},
		{"*.apps.example.net", "apps.example.net", false},
		{"*.apps.example.net", "a.b.apps.example.net", false},
		{"a.example.com", "a.example.com", true},
		{"a.example.com", "b.example.com", false},
	} {
		if got := covers(tc.serverName, tc.host); got != tc.want {
			t.Fatalf("covers(%q, %q) = %v, want %v", tc.serverName, tc.host, got, tc.want)
		}
	}
}
