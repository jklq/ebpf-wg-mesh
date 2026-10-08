package productionops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

func TestComponentProbeRetainsSelectedTLSAfterOperatorFilesDisappear(t *testing.T) {
	ca, err := certificate("probe-ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverIdentity, err := certificate("server", &ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	clientIdentity, err := certificate("probe-client", &ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(serverIdentity.Certificate, serverIdentity.Key)
	if err != nil {
		t.Fatal(err)
	}
	clients := x509.NewCertPool()
	clients.AppendCertsFromPEM(ca.Certificate)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"ready":true}`)) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: clients, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	origin, component := t.TempDir(), t.TempDir()
	for name, data := range map[string][]byte{"ca.crt": ca.Certificate, "client.crt": clientIdentity.Certificate, "client.key": clientIdentity.Key} {
		if err := writePrivate(origin+"/"+name, data); err != nil {
			t.Fatal(err)
		}
	}
	selected := Probe{URL: server.URL, CAFile: origin + "/ca.crt", CertFile: origin + "/client.crt", KeyFile: origin + "/client.key", Status: 200, BodyContains: `"ready":true`}
	probe, files, err := localizeProbe(selected, component)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := writePrivate(component+"/"+name, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveJSON(component+"/probe.json", probe); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	if err := Ready(context.Background(), deploy.Registry, "registry", component+"/probe.json"); err != nil {
		t.Fatal("actual TLS readiness still requires the operator files", err)
	}
	if err := writePrivate(probe.CAFile, []byte("corrupted trust root")); err != nil {
		t.Fatal(err)
	}
	if err := Ready(context.Background(), deploy.Registry, "registry", component+"/probe.json"); err == nil {
		t.Fatal("readiness accepted corrupt component-local TLS material")
	}
}
