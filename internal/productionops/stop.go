package productionops

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// envoyAdminAddress is the loopback admin listener rendered into every ingress
// bootstrap. Stop preparation and identity inspection run on the same host.
const envoyAdminAddress = "127.0.0.1:19000"

// prepareStop runs on a replica's host as systemd ExecStop=, before the process
// is signalled. systemd signals the process afterwards even if preparation
// fails, so every step is bounded and an exited process is not an error.
func prepareStop(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("prepare-stop requires a role")
	}
	fs := flag.NewFlagSet("prepare-stop "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "envoy":
		e := envoyStop{poll: 500 * time.Millisecond, client: &http.Client{Timeout: 5 * time.Second}}
		fs.StringVar(&e.admin, "admin", "http://"+envoyAdminAddress, "loopback Envoy admin URL")
		fs.DurationVar(&e.withdrawalMin, "withdrawal-min", 15*time.Second, "minimum time health checks fail before listeners drain")
		fs.DurationVar(&e.withdrawalMax, "withdrawal-max", 30*time.Second, "maximum time to wait for new ingress connections to stop")
		fs.DurationVar(&e.quiet, "quiet", 3*time.Second, "period without new ingress connections that confirms withdrawal")
		fs.DurationVar(&e.drain, "drain", 60*time.Second, "maximum time for active ingress connections to finish")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || e.withdrawalMin > e.withdrawalMax {
			return fmt.Errorf("invalid Envoy stop preparation arguments")
		}
		if err := loopbackHTTP(e.admin); err != nil {
			return err
		}
		e.replicated = replicated()
		return e.run(ctx)
	case "console":
		withdrawal := fs.Duration("withdrawal", 15*time.Second, "time readiness fails before the process is signalled")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || !filepath.IsAbs(fs.Arg(0)) {
			return fmt.Errorf("console stop preparation requires an absolute drain file")
		}
		if !replicated() {
			return nil // A sole replica keeps serving until SIGTERM; srvx then drains it.
		}
		return markDraining(ctx, fs.Arg(0), *withdrawal)
	default:
		return fmt.Errorf("no stop preparation for %s", args[0])
	}
}

// replicated reports whether the installer placed other replicas of this role.
// Withdrawal only helps when a balancer has somewhere else to send traffic; a
// sole replica would turn the wait into an outage. Unknown counts withdraw.
func replicated() bool {
	data, err := os.ReadFile(filepath.Join(os.Getenv("PLATFORM_CONFIG_DIR"), "replicas"))
	if err != nil {
		return true
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err != nil || count != 1
}

func loopbackHTTP(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("admin URL must be a plain loopback HTTP origin")
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("admin URL must be a plain loopback HTTP origin")
	}
	return nil
}

// markDraining names the systemd invocation being stopped. The console reports
// not ready only for that invocation, so balancers withdraw it while it still
// serves, and a restarted process never inherits the marker.
func markDraining(ctx context.Context, file string, withdrawal time.Duration) error {
	if pid := os.Getenv("MAINPID"); pid == "" || pid == "0" {
		return nil
	}
	invocation := strings.TrimSpace(os.Getenv("INVOCATION_ID"))
	if invocation == "" {
		return fmt.Errorf("stop preparation requires the systemd invocation")
	}
	if err := writePrivate(file, []byte(invocation+"\n")); err != nil {
		return err
	}
	return sleepContext(ctx, withdrawal)
}

type envoyStop struct {
	admin                        string
	withdrawalMin, withdrawalMax time.Duration
	quiet, drain, poll           time.Duration
	client                       *http.Client
	replicated                   bool
}

// ingressConnections sums the ingress listeners; healthChecks counts requests to
// the load-balancer health listener since Envoy started.
type ingressConnections struct{ total, active, requests, healthChecks int64 }

// Envoy closes its listeners immediately on SIGTERM, so it drains first. A
// replicated Envoy fails its health checks so balancers withdraw it, waits until
// new connections stop and lets active ones finish. Withdrawal is skipped when
// no balancer health-checks this Envoy, because nothing would act on it.
func (e envoyStop) run(ctx context.Context) error {
	if !e.replicated {
		return e.finishRequests(ctx)
	}
	before, err := e.connections(ctx)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil // Nothing is listening; the process already exited.
	}
	if err == nil && before.healthChecks == 0 {
		return e.drainListeners(ctx)
	}
	if err := e.post(ctx, "/healthcheck/fail"); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		return fmt.Errorf("fail Envoy health checks: %w", err)
	}
	start := time.Now()
	last := before
	quietSince := start
	for {
		if err := sleepContext(ctx, e.poll); err != nil {
			return err
		}
		current, readErr := e.connections(ctx)
		if errors.Is(readErr, syscall.ECONNREFUSED) {
			return nil
		}
		now := time.Now()
		if readErr != nil || err != nil || current.total != last.total {
			quietSince = now
		}
		last, err = current, readErr
		elapsed := now.Sub(start)
		if elapsed >= e.withdrawalMax || elapsed >= e.withdrawalMin && now.Sub(quietSince) >= e.quiet {
			break
		}
	}
	return e.drainListeners(ctx)
}

// drainListeners discourages and closes downstream connections, then waits for
// them to finish. Without skip_exit, Envoy exits by itself after its drain time.
func (e envoyStop) drainListeners(ctx context.Context) error {
	if err := e.post(ctx, "/drain_listeners?graceful&skip_exit"); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		return fmt.Errorf("drain Envoy listeners: %w", err)
	}
	return e.waitFor(ctx, e.drain, func(c ingressConnections) int64 { return c.active }, "connections")
}

// finishRequests stops a sole Envoy promptly. It keeps accepting connections,
// so waiting costs no availability, but only admitted requests are awaited:
// new connections keep arriving and would hold the restart for the whole
// drain period without making it any safer.
func (e envoyStop) finishRequests(ctx context.Context) error {
	return e.waitFor(ctx, min(e.drain, 10*time.Second), func(c ingressConnections) int64 { return c.requests }, "requests")
}

func (e envoyStop) waitFor(ctx context.Context, limit time.Duration, remaining func(ingressConnections) int64, what string) error {
	deadline := time.Now().Add(limit)
	for {
		current, err := e.connections(ctx)
		if errors.Is(err, syscall.ECONNREFUSED) || err == nil && remaining(current) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d ingress %s remain after the drain period", remaining(current), what)
		}
		if err := sleepContext(ctx, e.poll); err != nil {
			return err
		}
	}
}

func (e envoyStop) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(e.admin, "/")+path, nil)
	if err != nil {
		return err
	}
	res, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Envoy admin %s returned %s", path, res.Status)
	}
	return nil
}

// Ingress listeners use the ingress_http and ingress_https connection manager
// prefixes, the health listener ingress_health; the admin interface has none.
func (e envoyStop) connections(ctx context.Context) (ingressConnections, error) {
	var c ingressConnections
	filter := url.QueryEscape(`^http\.ingress_(https?|health)\.downstream_(cx_total|cx_active|rq_active|rq_total)$`)
	var stats struct {
		Stats []struct {
			Name  string       `json:"name"`
			Value *json.Number `json:"value"`
		} `json:"stats"`
	}
	if err := envoyAdminJSON(ctx, e.client, strings.TrimSuffix(e.admin, "/")+"/stats?format=json&filter="+filter, &stats); err != nil {
		return c, err
	}
	for _, stat := range stats.Stats {
		if stat.Value == nil {
			continue
		}
		value, err := stat.Value.Int64()
		if err != nil {
			return c, fmt.Errorf("invalid Envoy statistic %s", stat.Name)
		}
		switch name := stat.Name; {
		case strings.HasPrefix(name, "http.ingress_health."):
			if strings.HasSuffix(name, ".downstream_rq_total") {
				c.healthChecks += value
			}
		case strings.HasSuffix(name, ".downstream_cx_total"):
			c.total += value
		case strings.HasSuffix(name, ".downstream_cx_active"):
			c.active += value
		case strings.HasSuffix(name, ".downstream_rq_active"):
			c.requests += value
		}
	}
	return c, nil
}

func envoyAdminJSON(ctx context.Context, client *http.Client, target string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Envoy admin returned %s", res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(value)
}

// inspectEnvoyIdentity proves that Envoy's filesystem SDS loaded the installed
// client certificate: replacing the files alone does not establish that.
func inspectEnvoyIdentity(ctx context.Context, admin, certFile string) error {
	if err := loopbackHTTP(admin); err != nil {
		return err
	}
	data, err := os.ReadFile(certFile)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("installed ingress identity is not a PEM certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	var loaded struct {
		Certificates []struct {
			CertChain []struct {
				SerialNumber string `json:"serial_number"`
			} `json:"cert_chain"`
		} `json:"certificates"`
	}
	client := &http.Client{Timeout: 5 * time.Second}
	if err := envoyAdminJSON(ctx, client, strings.TrimSuffix(admin, "/")+"/certs", &loaded); err != nil {
		return err
	}
	for _, tlsContext := range loaded.Certificates {
		for _, cert := range tlsContext.CertChain {
			// Envoy prints the serial as hex and may keep a leading zero nibble.
			if serial, ok := new(big.Int).SetString(cert.SerialNumber, 16); ok && serial.Cmp(leaf.SerialNumber) == 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("Envoy has not loaded the installed ingress identity %x", leaf.SerialNumber)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
