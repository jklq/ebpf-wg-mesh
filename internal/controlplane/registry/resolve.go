package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func testDigestHex(seed string) string {
	sum := sha256.Sum256([]byte("deploy-by-digest-test:" + seed))
	return hex.EncodeToString(sum[:])
}

// ResolvedImage is a digest-pinned image identity. Agents pull Ref; the
// tag that produced it never runs.
type ResolvedImage struct {
	Repository     string
	ManifestDigest string
	Ref            string
}

// ErrImageNotFound reports that a registry has no such repository or tag.
var ErrImageNotFound = errors.New("image not found")

// ImageResolver pins an image reference to a manifest digest at deploy time.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string) (ResolvedImage, error)
}

// manifestAcceptTypes asks for a single manifest or an index. An index digest
// pins without platform selection; the runtime picks its own platform.
const manifestAcceptTypes = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json"

// HTTPResolver resolves tags through the OCI Distribution API with
// anonymous access. Direct images must be publicly pullable: 2.6 carries
// no private-registry credentials, and a Basic challenge fails closed with
// an actionable error instead of hanging a deploy on auth it cannot do.
//
// Registry traffic never follows redirects, and the transport re-validates
// every dial against DNS rebinding (see approvedDialTransport), except the
// token-realm authority of an operator-allowlisted registry.
type HTTPResolver struct {
	client *http.Client
	// baseTransport is the caller's transport before the dial guard wrapped it.
	baseTransport http.RoundTripper
	// allowedPrivateHosts are operator-declared registries reachable on
	// loopback, private, or link-local networks.
	allowedPrivateHosts []string
}

// NewHTTPResolver builds a resolver over client (or a default 15-second one
// when nil) with no-redirect policy and dial-time validation.
func NewHTTPResolver(client *http.Client, allowedPrivateHosts []string) *HTTPResolver {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	owned := *client
	owned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("registry redirect refused")
	}
	owned.Transport = approvedDialTransport(client.Transport, allowedPrivateHosts)
	return &HTTPResolver{client: &owned, baseTransport: client.Transport, allowedPrivateHosts: allowedPrivateHosts}
}

// clientForTrustedRealm returns a client for one request to a validated
// token-realm authority, with exactly that authority added to the approved
// hosts. The caller closes idle connections when done.
func (r *HTTPResolver) clientForTrustedRealm(authority string) *http.Client {
	approved := append(append([]string(nil), r.allowedPrivateHosts...), authority)
	clone := *r.client
	clone.Transport = approvedDialTransport(r.baseTransport, approved)
	return &clone
}

// approvedDialTransport wraps base's transport with a dialer that resolves
// registry hosts itself, validates the address, and connects to that exact
// address, defeating DNS rebinding between check and connect. TLS keeps
// validating the request hostname. Proxying is cleared so an environment
// proxy cannot resolve the target outside this guard. Non-dialing
// RoundTrippers (test stubs) pass through unchanged.
func approvedDialTransport(base http.RoundTripper, allowedPrivateHosts []string) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if base == nil {
		transport = http.DefaultTransport.(*http.Transport)
		ok = true
	}
	if !ok {
		return base
	}
	clone := transport.Clone()
	// A proxy would resolve the target outside the dial guard.
	clone.Proxy = nil
	next := clone.DialContext
	if next == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		next = d.DialContext
	}
	clone.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialApprovedAddress(ctx, network, addr, allowedPrivateHosts, next)
	}
	return clone
}

// dialApprovedAddress validates addr at every dial and connects to the
// checked IP directly; resolution failures fail closed.
func dialApprovedAddress(ctx context.Context, network, addr string, allowedPrivateHosts []string, next func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if registryHostAllowed(addr, allowedPrivateHosts) {
		return next(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("registry dial address %q is invalid: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("registry dial address %q resolves to a prohibited private destination; list it in CONTROLPLANE_DIRECT_IMAGE_ALLOWED_PRIVATE_REGISTRIES if the control plane should reach it", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if prohibitedIP(ip) {
			return nil, fmt.Errorf("registry dial address %q resolves to a prohibited private destination; list it in CONTROLPLANE_DIRECT_IMAGE_ALLOWED_PRIVATE_REGISTRIES if the control plane should reach it", addr)
		}
		return next(ctx, network, addr)
	}
	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("registry dial host %q cannot be verified: %w", addr, err)
	}
	var lastDialErr error
	for _, candidate := range addrs {
		if prohibitedIP(candidate.IP) {
			continue
		}
		conn, err := next(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastDialErr = err
	}
	if lastDialErr != nil {
		return nil, fmt.Errorf("registry dial address %q failed on every permitted address: %w", addr, lastDialErr)
	}
	return nil, fmt.Errorf("registry dial address %q resolves to a prohibited private destination; list it in CONTROLPLANE_DIRECT_IMAGE_ALLOWED_PRIVATE_REGISTRIES if the control plane should reach it", addr)
}

func (r *HTTPResolver) Resolve(ctx context.Context, ref string) (ResolvedImage, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return ResolvedImage{}, err
	}
	if parsed.Pinned() {
		return ResolvedImage{Repository: parsed.Repository, ManifestDigest: parsed.Digest, Ref: parsed.PinnedRef()}, nil
	}
	host, path, _ := strings.Cut(parsed.Repository, "/")
	if err := permittedRegistryDestination(ctx, host, r.allowedPrivateHosts); err != nil {
		return ResolvedImage{}, err
	}
	manifestURL := manifestURLFor(parsed.Repository, parsed.Tag)
	digest, err := r.manifestDigest(ctx, manifestURL, "")
	if unauthorizedScopeError(err) {
		return ResolvedImage{}, err
	}
	if challenge, ok := bearerChallenge(err); ok {
		token, tokenErr := r.anonymousToken(ctx, challenge, host, path)
		if tokenErr != nil {
			return ResolvedImage{}, tokenErr
		}
		digest, err = r.manifestDigest(ctx, manifestURL, token)
	}
	if err != nil {
		return ResolvedImage{}, err
	}
	// Canonicalize registry-provided digests like user input.
	digest = strings.ToLower(strings.TrimSpace(digest))
	if err := ValidateManifestDigest(digest); err != nil {
		return ResolvedImage{}, fmt.Errorf("registry %s returned an invalid manifest digest for %s: %w", host, ref, err)
	}
	return ResolvedImage{Repository: parsed.Repository, ManifestDigest: digest, Ref: parsed.Repository + "@" + digest}, nil
}

func registryScheme(host string) string {
	name := hostnameOf(host)
	if strings.EqualFold(name, "localhost") {
		return "http"
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return "http"
	}
	return "https"
}

// registryEndpoint maps a repository host to its OCI Distribution API
// endpoint. Docker Hub's API lives at registry-1.docker.io.
func registryEndpoint(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	default:
		return host
	}
}

func manifestURLFor(repository, reference string) string {
	host, path, _ := strings.Cut(repository, "/")
	endpoint := registryEndpoint(host)
	return registryScheme(endpoint) + "://" + endpoint + "/v2/" + path + "/manifests/" + reference
}

type bearerAuthError struct {
	challenge bearerChallengeValues
}

func (e *bearerAuthError) Error() string { return "registry requires authentication" }

type bearerChallengeValues struct {
	realm   string
	service string
}

func bearerChallenge(err error) (bearerChallengeValues, bool) {
	var authErr *bearerAuthError
	if !errors.As(err, &authErr) {
		return bearerChallengeValues{}, false
	}
	return authErr.challenge, true
}

func unauthorizedScopeError(err error) bool {
	var scopeErr *registryScopeError
	return errors.As(err, &scopeErr)
}

type registryScopeError struct{ msg string }

func (e *registryScopeError) Error() string { return e.msg }

func (r *HTTPResolver) manifestDigest(ctx context.Context, manifestURL, token string) (string, error) {
	digest, status, wwwAuth, err := r.manifestRequest(ctx, http.MethodHead, manifestURL, token)
	if err != nil {
		return "", err
	}
	if status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		digest, status, wwwAuth, err = r.manifestRequest(ctx, http.MethodGet, manifestURL, token)
		if err != nil {
			return "", err
		}
	}
	switch {
	case status == http.StatusOK:
		if digest == "" {
			return "", fmt.Errorf("registry response for %s omitted the Docker-Content-Digest header", manifestURL)
		}
		return digest, nil
	case status == http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrImageNotFound, manifestURL)
	case status == http.StatusUnauthorized:
		return "", r.authError(manifestURL, wwwAuth)
	default:
		return "", fmt.Errorf("resolve %s: registry returned status %d", manifestURL, status)
	}
}

// manifestRequest performs one manifest HEAD or GET. Redirects are refused.
func (r *HTTPResolver) manifestRequest(ctx context.Context, method, manifestURL, token string) (digest string, status int, wwwAuthenticate string, err error) {
	req, err := http.NewRequestWithContext(ctx, method, manifestURL, nil)
	if err != nil {
		return "", 0, "", fmt.Errorf("resolve %s: %w", manifestURL, err)
	}
	req.Header.Set("Accept", manifestAcceptTypes)
	req.Header.Set("User-Agent", "ebpf-wg-mesh-deploy-by-digest/1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", 0, "", fmt.Errorf("resolve %s: %w", manifestURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.Header.Get("Docker-Content-Digest"), resp.StatusCode, resp.Header.Get("WWW-Authenticate"), nil
}

func (r *HTTPResolver) authError(manifestURL, wwwAuthenticate string) error {
	challenge := strings.TrimSpace(wwwAuthenticate)
	if challenge == "" {
		return &registryScopeError{msg: fmt.Sprintf("resolve %s: registry denied access without an authentication challenge", manifestURL)}
	}
	lowered := strings.ToLower(challenge)
	if strings.HasPrefix(lowered, "basic") {
		return &registryScopeError{msg: fmt.Sprintf("resolve %s: registry requires credentials; direct images must be publicly pullable", manifestURL)}
	}
	if !strings.HasPrefix(lowered, "bearer ") && !strings.HasPrefix(lowered, "token ") {
		return &registryScopeError{msg: fmt.Sprintf("resolve %s: unsupported registry auth challenge %q", manifestURL, challenge)}
	}
	values := parseAuthChallenge(challenge[strings.IndexByte(challenge, ' ')+1:])
	if values.realm == "" {
		return &registryScopeError{msg: fmt.Sprintf("resolve %s: registry auth challenge has no realm", manifestURL)}
	}
	return &bearerAuthError{challenge: values}
}

func parseAuthChallenge(params string) bearerChallengeValues {
	var values bearerChallengeValues
	for _, part := range strings.Split(params, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			values.realm = value
		case "service":
			values.service = value
		}
	}
	return values
}

// anonymousToken fetches an anonymous pull token from a Bearer challenge's
// realm. The realm is validated against the issuing registry first and the
// fetch follows no redirects.
func (r *HTTPResolver) anonymousToken(ctx context.Context, challenge bearerChallengeValues, registryHost, repository string) (string, error) {
	realm, err := tokenRealmURL(ctx, challenge.realm, registryHost, r.allowedPrivateHosts)
	if err != nil {
		return "", err
	}
	client := r.client
	if realm.trustedAuthority {
		client = r.clientForTrustedRealm(realm.url.Host)
		defer client.CloseIdleConnections()
	}
	tokenURL := realm.url
	query := tokenURL.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	query.Set("scope", "repository:"+repository+":pull")
	tokenURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	req.Header.Set("User-Agent", "ebpf-wg-mesh-deploy-by-digest/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch registry token: token endpoint returned status %d", resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", errors.New("fetch registry token: token endpoint returned no token")
	}
	return token, nil
}

// StaticResolver pins tags from an explicit map for tests and offline
// environments. Tags without an entry fall back to Fallback when set.
type StaticResolver struct {
	Tags     map[string]string
	Fallback func(ref string) (string, bool)
}

// StaticResolverForTest pins every tag to the sha256 of its normalized form.
func StaticResolverForTest() *StaticResolver {
	return &StaticResolver{Tags: map[string]string{}, Fallback: func(ref string) (string, bool) {
		parsed, err := ParseReference(ref)
		if err != nil || parsed.Pinned() {
			return "", false
		}
		return "sha256:" + testDigestHex(parsed.Repository+":"+parsed.Tag), true
	}}
}

func (s StaticResolver) Resolve(_ context.Context, ref string) (ResolvedImage, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return ResolvedImage{}, err
	}
	if parsed.Pinned() {
		return ResolvedImage{Repository: parsed.Repository, ManifestDigest: parsed.Digest, Ref: parsed.PinnedRef()}, nil
	}
	key := parsed.Repository + ":" + parsed.Tag
	digest := s.Tags[strings.TrimSpace(ref)]
	if digest == "" {
		digest = s.Tags[key]
	}
	if digest == "" && s.Fallback != nil {
		var ok bool
		if digest, ok = s.Fallback(strings.TrimSpace(ref)); !ok {
			digest = ""
		}
	}
	if digest == "" {
		return ResolvedImage{}, fmt.Errorf("%w: no digest configured for %s", ErrImageNotFound, ref)
	}
	if err := ValidateManifestDigest(digest); err != nil {
		return ResolvedImage{}, fmt.Errorf("static digest for %s: %w", ref, err)
	}
	return ResolvedImage{Repository: parsed.Repository, ManifestDigest: digest, Ref: parsed.Repository + "@" + digest}, nil
}
