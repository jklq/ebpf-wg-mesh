package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/apiclient"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// HTTPOptions configures the streamable HTTP transport.
type HTTPOptions struct {
	// APIURL is the public API origin requests are forwarded to.
	APIURL string
	// PublicURL is this MCP endpoint's public URL (for protected resource
	// metadata), e.g. https://mcp.example.test/mcp. When set, the
	// metadata document is served and linked from WWW-Authenticate.
	PublicURL string
	Defaults  Defaults
}

// NewHTTPHandler serves the MCP server over streamable HTTP at the
// handler's root. Every request must carry a platform API token as a
// Bearer credential; tokens are validated against the public API (60s
// cache) and each session gets an API client bound to its own token, so
// no shared credential ever exists server-side. The handler is
// stateless: any replica can serve any request.
func NewHTTPHandler(opts HTTPOptions) (http.Handler, error) {
	if strings.TrimSpace(opts.APIURL) == "" {
		return nil, fmt.Errorf("mcp http: api url is required")
	}
	verifier := &tokenVerifier{apiURL: opts.APIURL, cache: map[string]cacheEntry{}}
	streamable := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		secret, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		client := apiclient.New(opts.APIURL, strings.TrimSpace(secret))
		client.UserAgent = "ebpf-wg-mesh-mcp/" + Version
		server, err := New(client, opts.Defaults)
		if err != nil {
			return nil
		}
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, SessionTimeout: 30 * time.Minute})

	requireOptions := &auth.RequireBearerTokenOptions{AllowMissingExpiration: true}
	if opts.PublicURL != "" {
		base := strings.TrimSuffix(opts.PublicURL, "/mcp")
		requireOptions.ResourceMetadataURL = base + "/.well-known/oauth-protected-resource/mcp"
	}
	protected := auth.RequireBearerToken(verifier.verify, requireOptions)(streamable)

	mux := http.NewServeMux()
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
	if opts.PublicURL != "" {
		mux.Handle("/.well-known/oauth-protected-resource/mcp", auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               opts.PublicURL,
			ScopesSupported:        []string{"read", "write"},
			BearerMethodsSupported: []string{"header"},
		}))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return mux, nil
}

type cacheEntry struct {
	info    *auth.TokenInfo
	expires time.Time
}

type tokenVerifier struct {
	apiURL string
	mu     sync.Mutex
	cache  map[string]cacheEntry
}

func (v *tokenVerifier) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	keyBytes := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(keyBytes[:])
	now := time.Now()
	v.mu.Lock()
	if entry, ok := v.cache[key]; ok && now.Before(entry.expires) {
		info := entry.info
		v.mu.Unlock()
		return info, nil
	}
	v.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := apiclient.New(v.apiURL, token).Whoami(callCtx)
	if err != nil {
		if apiclient.IsCode(err, "unauthenticated") {
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		return nil, err
	}
	scopes := []string{"read"}
	if info.Access == "full" {
		scopes = append(scopes, "write")
	}
	tokenInfo := &auth.TokenInfo{Scopes: scopes, UserID: info.UserID}
	if info.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, info.ExpiresAt); err == nil {
			tokenInfo.Expiration = parsed
		}
	}
	v.mu.Lock()
	if len(v.cache) > 1024 {
		v.cache = map[string]cacheEntry{}
	}
	v.cache[key] = cacheEntry{info: tokenInfo, expires: now.Add(time.Minute)}
	v.mu.Unlock()
	return tokenInfo, nil
}
