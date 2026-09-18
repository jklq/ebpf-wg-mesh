package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/localteststack"
)

func cloudflareTunnelRequested(explicit, githubConfigured bool, token, hostname string) bool {
	return explicit ||
		(githubConfigured && strings.TrimSpace(token) != "" && strings.TrimSpace(hostname) != "")
}

func startCloudflareTunnel(ctx context.Context, tunnelToken string, hostname string) (localteststack.PublicURLResult, error) {
	tunnelCtx, cancelTunnel := context.WithCancel(ctx)

	type result struct {
		publicURL localteststack.PublicURLResult
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		publicURL, err := localteststack.StartCloudflareTunnel(tunnelCtx, tunnelToken, hostname)
		resultCh <- result{publicURL: publicURL, err: err}
	}()

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		cancelTunnel()
		return localteststack.PublicURLResult{}, ctx.Err()
	case <-timer.C:
		cancelTunnel()
		return localteststack.PublicURLResult{}, context.DeadlineExceeded
	case result := <-resultCh:
		if result.err != nil {
			cancelTunnel()
			return localteststack.PublicURLResult{}, result.err
		}
		closeFn := result.publicURL.Close
		result.publicURL.Close = func() error {
			cancelTunnel()
			if closeFn != nil {
				return closeFn()
			}
			return nil
		}
		return result.publicURL, nil
	}
}

func missingCloudflareRuntimeKeys(tunnelToken string, hostname string) []string {
	missing := make([]string, 0, 2)
	if strings.TrimSpace(tunnelToken) == "" {
		missing = append(missing, localteststack.CloudflareTunnelTokenKey)
	}
	if strings.TrimSpace(hostname) == "" {
		missing = append(missing, localteststack.CloudflareHostnameKey)
	}
	return missing
}

func describeOnePasswordLoadError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "load 1Password environment: timed out after 30s; use OP_SERVICE_ACCOUNT_TOKEN or fix desktop-app integration"
	case errors.Is(err, context.Canceled):
		return "load 1Password environment: startup interrupted"
	}

	message := err.Error()
	if strings.Contains(message, "Account not found") {
		return fmt.Sprintf(
			"load 1Password environment: desktop-app auth could not find %s; set %s to the exact account UUID/name shown by 1Password, or use %s instead",
			localteststack.OPAccountKey,
			localteststack.OPAccountKey,
			localteststack.OPServiceAccountTokenKey,
		)
	}
	if strings.Contains(message, "invalid service account token") ||
		strings.Contains(message, "base64 decoding failed") ||
		strings.Contains(message, "service account token") {
		return fmt.Sprintf(
			"load 1Password environment: invalid %s (SDK could not decode it). For local dev, prefer a 1Password Environments-mounted repo-root .env with the GitHub/devstack keys and unset %s/%s; for headless SDK use, set %s to a full token starting with ops_ from a 1Password service account",
			localteststack.OPServiceAccountTokenKey,
			localteststack.OPEnvironmentIDKey,
			localteststack.OPServiceAccountTokenKey,
			localteststack.OPServiceAccountTokenKey,
		)
	}
	return fmt.Sprintf("load 1Password environment: %v", err)
}

func describeCloudflareStartupError(err error, hostname string, secrets ...string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("start cloudflare tunnel for %s: timed out after 30s; verify `cloudflared` is installed, then confirm %s points at a pre-provisioned Cloudflare Tunnel", hostname, localteststack.CloudflareHostnameKey)
	case errors.Is(err, context.Canceled):
		return "start cloudflare tunnel: startup interrupted"
	}

	message := err.Error()
	switch {
	case strings.Contains(message, localteststack.CloudflareTunnelTokenKey+" is required"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: %s is required; export it from 1Password env or your shell before running `make dev-ephemeral`",
			hostname,
			localteststack.CloudflareTunnelTokenKey,
		)
	case strings.Contains(message, localteststack.CloudflareHostnameKey+" is required"):
		return fmt.Sprintf(
			"start cloudflare tunnel: %s is required; export it from 1Password env or your shell before running `make dev-ephemeral`",
			localteststack.CloudflareHostnameKey,
		)
	case strings.Contains(message, "start cloudflared"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: cloudflared not found; install it locally, then rerun `make dev-ephemeral`",
			hostname,
		)
	case strings.Contains(message, "cloudflared exited"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: cloudflared exited before becoming ready; check the token, hostname, and Cloudflare tunnel routing",
			hostname,
		)
	case strings.Contains(message, "invalid "+localteststack.CloudflareHostnameKey):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: invalid %s; use a bare hostname such as `mesh.example.test`",
			hostname,
			localteststack.CloudflareHostnameKey,
		)
	default:
		return fmt.Sprintf("start cloudflare tunnel for %s: %v", hostname, localteststack.RedactError(err, secrets...))
	}
}
