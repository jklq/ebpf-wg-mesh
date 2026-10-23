package activator

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type fakeWaker struct {
	endpoints []Endpoint
	ready     bool
	err       error
	hosts     []string
}

func (f *fakeWaker) Wake(_ context.Context, hostname string) ([]Endpoint, bool, error) {
	f.hosts = append(f.hosts, hostname)
	return f.endpoints, f.ready, f.err
}

func upstreamEndpoint(t *testing.T, srv *httptest.Server) Endpoint {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return Endpoint{Address: host, Port: int32(port)}
}

func TestActivatorHoldsAndProxiesFirstRequest(t *testing.T) {
	t.Parallel()

	var gotHost, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("workload:" + string(body)))
	}))
	defer upstream.Close()

	waker := &fakeWaker{endpoints: []Endpoint{upstreamEndpoint(t, upstream)}, ready: true}
	frontend := httptest.NewServer(NewServer(waker, Config{}).Handler())
	defer frontend.Close()

	req, err := http.NewRequest(http.MethodPost, frontend.URL+"/some/path?q=1", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "App.Example.com:8443"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "workload:ping" {
		t.Fatalf("status=%d body=%q, want proxied workload response", resp.StatusCode, body)
	}
	if len(waker.hosts) != 1 || waker.hosts[0] != "app.example.com" {
		t.Fatalf("wake hosts = %v, want [app.example.com]", waker.hosts)
	}
	if gotHost != "App.Example.com:8443" {
		t.Fatalf("upstream saw Host %q, want the public hostname preserved", gotHost)
	}
	if gotPath != "/some/path" {
		t.Fatalf("upstream saw path %q", gotPath)
	}
}

func TestActivatorWakeOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		waker      *fakeWaker
		wantStatus int
		wantRetry  bool
	}{
		{name: "not found", waker: &fakeWaker{err: ErrNotFound}, wantStatus: http.StatusNotFound},
		{name: "wake error", waker: &fakeWaker{err: context.DeadlineExceeded}, wantStatus: http.StatusBadGateway},
		{name: "still waking", waker: &fakeWaker{ready: false}, wantStatus: http.StatusServiceUnavailable, wantRetry: true},
		{name: "ready without endpoints", waker: &fakeWaker{ready: true}, wantStatus: http.StatusServiceUnavailable, wantRetry: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frontend := httptest.NewServer(NewServer(tc.waker, Config{}).Handler())
			defer frontend.Close()
			req, err := http.NewRequest(http.MethodGet, frontend.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "svc.example.com"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantRetry && resp.Header.Get("Retry-After") == "" {
				t.Fatal("expected Retry-After header")
			}
		})
	}
}
