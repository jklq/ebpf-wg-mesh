package productionops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

func TestProviderVisibilityIsNotPowerFencing(t *testing.T) {
	for _, tc := range []struct {
		name, kind, response string
		status               int
		fenced               bool
	}{
		{"hetzner-not-found", "hetzner", `{}`, 404, false},
		{"gigahost-not-found", "gigahost", `{}`, 404, false},
		{"gigahost-filtered", "gigahost", `{"meta":{"status":200},"data":[]}`, 200, false},
		{"hetzner-power-off", "hetzner", `{"server":{"id":1,"status":"off"}}`, 200, true},
		{"gigahost-power-off", "gigahost", `{"meta":{"status":200},"data":[{"srv_id":"1","srv_status":false}]}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/servers/1" {
					t.Error("unexpected provider request", req.Method, req.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.response))
			}))
			defer api.Close()
			r := testRunner(t)
			r.Plan.Installation.Hosts[0].Binding.ServerID = "1"
			r.Plan.Placements = []deploy.Placement{{Role: deploy.Database, Host: "a", Instance: "db-a"}}
			r.Adapter = func(deploy.Installation, deploy.Host) (deploy.Adapter, error) {
				return &deploy.APIAdapter{Kind: tc.kind, BaseURL: api.URL, Token: "isolated", Client: api.Client()}, nil
			}
			r.Remote = inventoryPeerRemote{Offline: map[string]bool{"a": true}}
			err := r.verifyHostFenced(context.Background(), r.Plan, "a")
			if (err == nil) != tc.fenced {
				t.Fatal("provider visibility was accepted as a power observation", err)
			}
		})
	}
}
