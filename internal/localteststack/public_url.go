package localteststack

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	CloudflareTunnelTokenKey = "CLOUDFLARE_TUNNEL_TOKEN"
	CloudflareHostnameKey    = "CLOUDFLARE_HOSTNAME"
)

type PublicURLResult struct {
	BaseURL string
	Host    string
	Close   func() error
}

func StartCloudflareTunnel(ctx context.Context, tunnelToken string, hostname string) (PublicURLResult, error) {
	token := strings.TrimSpace(tunnelToken)
	if token == "" {
		return PublicURLResult{}, fmt.Errorf("%s is required", CloudflareTunnelTokenKey)
	}
	hostname = strings.Trim(strings.TrimSpace(hostname), ".")
	if hostname == "" {
		return PublicURLResult{}, fmt.Errorf("%s is required", CloudflareHostnameKey)
	}

	baseURL, err := parsePublicBaseURL("https://" + hostname)
	if err != nil {
		return PublicURLResult{}, fmt.Errorf("invalid %s %q: %w", CloudflareHostnameKey, hostname, err)
	}

	cmd := exec.CommandContext(ctx, "cloudflared", "tunnel", "run", "--token", token)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return PublicURLResult{}, fmt.Errorf("start cloudflared: %w", err)
	}

	if err := waitForCommandStartup(ctx, cmd, 5*time.Second); err != nil {
		_ = stopCommand(cmd)
		return PublicURLResult{}, err
	}

	return PublicURLResult{
		BaseURL: baseURL.String(),
		Host:    baseURL.Host,
		Close: func() error {
			return stopCommand(cmd)
		},
	}, nil
}

func parsePublicBaseURL(raw string) (*url.URL, error) {
	parsed, err := parseHTTPSBaseURL(raw)
	if err != nil {
		return nil, err
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func waitForCommandStartup(ctx context.Context, cmd *exec.Cmd, startupDelay time.Duration) error {
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	timer := time.NewTimer(startupDelay)
	defer timer.Stop()

	select {
	case err := <-waitCh:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("cloudflared exited: %w", err)
		}
		return fmt.Errorf("cloudflared exited before becoming ready")
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func stopCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
		return err
	}
	_, _ = cmd.Process.Wait()
	return nil
}
