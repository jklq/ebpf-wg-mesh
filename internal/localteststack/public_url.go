package localteststack

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	ngrok "golang.ngrok.com/ngrok/v2"
)

const (
	ngrokClientName    = "ebpf-wg-mesh"
	ngrokClientVersion = "localteststack"
	NgrokAuthtokenKey  = "NGROK_AUTHTOKEN"
	NgrokDomainKey     = "NGROK_DOMAIN"
)

type PublicTunnel interface {
	URL() *url.URL
	Close() error
}

type PublicTunnelStarter interface {
	Start(ctx context.Context, upstream string, domain string) (PublicTunnel, error)
}

type PublicURLResult struct {
	BaseURL string
	Host    string
	Close   func() error
}

type NgrokTunnelStarter struct {
	Authtoken string
}

func (s NgrokTunnelStarter) Start(ctx context.Context, upstream string, domain string) (PublicTunnel, error) {
	token := strings.TrimSpace(s.Authtoken)
	if token == "" {
		return nil, fmt.Errorf("ngrok authtoken is required")
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil, fmt.Errorf("ngrok domain is required")
	}
	agent, err := ngrok.NewAgent(
		ngrok.WithAuthtoken(token),
		ngrok.WithClientInfo(ngrokClientName, ngrokClientVersion),
		ngrok.WithAgentDescription("ebpf-wg-mesh localteststack"),
	)
	if err != nil {
		return nil, err
	}
	return agent.Forward(ctx, ngrok.WithUpstream(upstream), ngrok.WithURL(domain))
}

func ResolvePublicURLForUpstream(ctx context.Context, ngrokAuthtoken string, ngrokDomain string, upstream string, starter PublicTunnelStarter) (PublicURLResult, error) {
	if strings.TrimSpace(ngrokAuthtoken) == "" {
		return PublicURLResult{}, fmt.Errorf("NGROK_AUTHTOKEN is required")
	}
	if strings.TrimSpace(ngrokDomain) == "" {
		return PublicURLResult{}, fmt.Errorf("NGROK_DOMAIN is required")
	}
	if strings.TrimSpace(upstream) == "" {
		return PublicURLResult{}, fmt.Errorf("ingress upstream is required")
	}
	if starter == nil {
		starter = NgrokTunnelStarter{Authtoken: ngrokAuthtoken}
	}
	tunnel, err := starter.Start(ctx, upstream, ngrokDomain)
	if err != nil {
		return PublicURLResult{}, fmt.Errorf("start ngrok tunnel: %w", err)
	}
	if tunnel == nil {
		return PublicURLResult{}, fmt.Errorf("ngrok tunnel starter returned no tunnel")
	}
	if tunnel.URL() == nil {
		_ = tunnel.Close()
		return PublicURLResult{}, fmt.Errorf("ngrok tunnel did not return a public URL")
	}

	baseURL, err := parsePublicBaseURL(tunnel.URL().String())
	if err != nil {
		_ = tunnel.Close()
		return PublicURLResult{}, err
	}

	return PublicURLResult{
		BaseURL: baseURL.String(),
		Host:    baseURL.Host,
		Close:   tunnel.Close,
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
