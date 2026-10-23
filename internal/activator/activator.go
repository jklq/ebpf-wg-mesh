// Package activator implements the wake activator that sits next to each
// Envoy ingress instance. Envoy routes hostnames of sleeping serverless
// services here; the activator asks the control plane to wake the service,
// holds the request, and proxies it to the first ready endpoint. See
// docs/serverless.md.
package activator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"time"
)

// Endpoint is one ready workload address returned by a wake.
type Endpoint struct {
	Address string
	Port    int32
}

// ErrNotFound reports a hostname with no live domain binding.
var ErrNotFound = errors.New("no live domain binding for hostname")

// Waker wakes the service behind a hostname and waits for readiness.
type Waker interface {
	Wake(ctx context.Context, hostname string) (endpoints []Endpoint, ready bool, err error)
}

// Config configures the activator HTTP server.
type Config struct {
	// WakeTimeout bounds one wake call; it should match the control
	// plane's serverless wake deadline.
	WakeTimeout time.Duration
}

type Server struct {
	waker   Waker
	timeout time.Duration
	next    atomic.Uint64
	proxy   *httputil.ReverseProxy
}

func NewServer(waker Waker, cfg Config) *Server {
	timeout := cfg.WakeTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	s := &Server{waker: waker, timeout: timeout}
	s.proxy = &httputil.ReverseProxy{
		// ErrorHandler only: every request rewrites to the endpoint chosen
		// for it in ServeHTTP via a per-request cloned proxy director.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("activator proxy failed", "host", r.Host, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		// Held cold starts and long-lived responses must not buffer.
		FlushInterval: -1,
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", s.serve)
	return mux
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	hostname := r.Host
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = host
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	endpoints, ready, err := s.waker.Wake(ctx, hostname)
	cancel()
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case err != nil:
		slog.Warn("activator wake failed", "host", hostname, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	case !ready || len(endpoints) == 0:
		// The wake continues in the background; ask the client to retry.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "service is waking up, please retry", http.StatusServiceUnavailable)
		return
	}
	endpoint := endpoints[int(s.next.Add(1)-1)%len(endpoints)]
	target := net.JoinHostPort(endpoint.Address, fmt.Sprint(endpoint.Port))
	proxy := *s.proxy
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = target
		// req.Host stays the public hostname, exactly as Envoy presents
		// it to the workload on the normal route.
	}
	proxy.ServeHTTP(w, r)
}
