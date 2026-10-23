// Command activator wakes sleeping serverless services on behalf of the
// Envoy ingress it is co-located with, holding the first request until an
// endpoint is ready and proxying it there. See docs/serverless.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/activator"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "HTTP listen address Envoy dials for asleep hostnames")
	addrsFlag := flag.String("controlplane-addresses", "", "comma-separated control-plane internal gRPC addresses (host:port)")
	identityDir := flag.String("identity-dir", "", "provisioned activator identity directory (generation layout)")
	serverName := flag.String("server-name", "", "control plane certificate SAN to verify")
	wakeTimeout := flag.Duration("wake-timeout", 90*time.Second, "maximum time to hold a request while its service wakes")
	flag.Parse()

	if err := run(*listen, *addrsFlag, *identityDir, *serverName, *wakeTimeout); err != nil {
		log.Fatal(err)
	}
}

func run(listen, addrsFlag, identityDir, serverName string, wakeTimeout time.Duration) error {
	var addrs []string
	for _, addr := range strings.Split(addrsFlag, ",") {
		if trimmed := strings.TrimSpace(addr); trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	if len(addrs) == 0 || identityDir == "" || serverName == "" {
		return fmt.Errorf("--controlplane-addresses, --identity-dir, and --server-name are required")
	}
	waker, err := activator.NewControlPlaneWaker(activator.ControlPlaneConfig{
		Addresses: addrs, IdentityDir: identityDir, ServerName: serverName,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           activator.NewServer(waker, activator.Config{WakeTimeout: wakeTimeout}).Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("activator listening", "addr", listen, "controlplane", addrs)
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
