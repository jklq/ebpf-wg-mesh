package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGigahostImportsScopedServersAndUsesDocumentedLifecycle(t *testing.T) {
	var requests []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer scoped-server-key" {
			t.Error("missing scoped key")
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/servers/3523":
			w.Write([]byte(`{"meta":{"status":200},"data":[{"srv_id":"3523","srv_primary_ip":"192.0.2.24","srv_status":true}]}`))
		case "/servers/3523/on", "/servers/3523/off", "/servers/3523/reboot":
			if r.Method != "GET" {
				t.Error("power operations must follow documented GET endpoints")
			}
			w.Write([]byte(`{"meta":{"status":200}}`))
		case "/servers/3523/cancel":
			var body map[string]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["early_termination"] != 1 {
				t.Error("wrong cancel body")
			}
			w.Write([]byte(`{"meta":{"status":200}}`))
		default:
			t.Error("undocumented operation", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	a := &APIAdapter{Kind: "gigahost", BaseURL: s.URL, Token: "scoped-server-key", Client: s.Client()}
	h := Host{Binding: Binding{ServerID: "3523"}}
	server, found, err := a.Discover(context.Background(), "production", h)
	if err != nil || !found || server.Address != "192.0.2.24" {
		t.Fatal(server, found, err)
	}
	if a.Capabilities().Create {
		t.Fatal("undocumented creation exposed")
	}
	if _, err := a.Create(context.Background(), "production", h); err == nil {
		t.Fatal("Gigahost purchase accepted")
	}
	for _, action := range []string{"on", "off", "reboot"} {
		if err := a.Power(context.Background(), "3523", action); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Delete(context.Background(), "3523"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 {
		t.Fatal(requests)
	}
}
func TestHetznerDiscoversPurchaseByInstallationLabels(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("label_selector") != "platform-installation=production,platform-host=home" {
			t.Error(r.URL.RawQuery)
		}
		w.Write([]byte(`{"servers":[{"id":42,"status":"running","public_net":{"ipv4":{"ip":"192.0.2.42"}}}]}`))
	}))
	defer s.Close()
	a := &APIAdapter{Kind: "hetzner", BaseURL: s.URL, Token: "token", Client: s.Client()}
	server, found, err := a.Discover(context.Background(), "production", Host{ID: "home"})
	if err != nil || !found || server.ID != "42" {
		t.Fatal(server, found, err)
	}
}
func TestGigahostEmbeddedAPIFailureIsNotSuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"meta":{"status":403},"data":[]}`)) }))
	defer s.Close()
	a := &APIAdapter{Kind: "gigahost", BaseURL: s.URL, Client: s.Client()}
	if _, _, err := a.Discover(context.Background(), "production", Host{Binding: Binding{ServerID: "3523"}}); err == nil {
		t.Fatal("permission failure treated as missing server")
	}
}
