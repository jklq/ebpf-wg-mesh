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

// ResolvedImage is a digest-pinned image identity: the normalized repository
// plus the manifest digest the tag pointed at when it was resolved. Agents
// pull Ref; the tag that produced it is user input and never runs.
type ResolvedImage struct {
	Repository     string
	ManifestDigest string
	Ref            string
}

// ErrImageNotFound reports that a registry has no such repository or tag.
var ErrImageNotFound = errors.New("image not found")

// ImageResolver pins an image reference to a manifest digest at deploy
// time. References that already carry a digest never touch the network;
// tags are resolved against the registry that owns them.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string) (ResolvedImage, error)
}

// manifestAcceptTypes asks for either a single manifest or an index. An
// index digest pins without platform selection: the agent pulls the index
// by digest and the runtime picks its own platform. 2.6 deliberately
// performs no multi-arch selection here.
const manifestAcceptTypes = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json"

// HTTPResolver resolves tags through the OCI Distribution API with
// anonymous access. Direct images must be publicly pullable: 2.6 carries
// no private-registry credentials, and a Basic challenge fails closed with
// an actionable error instead of hanging a deploy on auth it cannot do.
//
// Registry traffic never follows redirects: a Location header from a
// user-chosen registry is attacker-controlled input just like a Bearer
// realm, and following one would let the registry walk the control plane
// toward internal URLs. Real registries serve manifest HEAD/GET from the
// canonical endpoint directly (redirects are a blob-CDN concern, and this
// resolver never fetches blobs), so refusal fails closed without breaking
// legitimate resolution.
//
// The registry host itself is user-controlled input too, so it must be a
// public destination unless the operator allowlisted it as an internal
// registry the control plane may reach (see permittedRegistryDestination).
// That check alone is not enough: DNS rebinding — the host answering the
// check with a public address and the later connection with a private one —
// would swap the address after validation, so the resolver's transport
// re-validates at every dial and connects to the approved address directly
// (see approvedDialTransport).
type HTTPResolver struct {
	client *http.Client
	// allowedPrivateHosts are registry hosts (host[:port]) the operator
	// declared reachable even on loopback, private, or link-local
	// networks.
	allowedPrivateHosts []string
}

// NewHTTPResolver builds a registry resolver over client, or a default
// 15-second client when nil. The client is copied so the resolver can
// enforce its no-redirect policy without touching the caller's client.
// The transport is cloned and pinned through approvedDialTransport so
// every connection — registry or token realm — is validated and pinned
// at dial time.
func NewHTTPResolver(client *http.Client, allowedPrivateHosts []string) *HTTPResolver {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	owned := *client
	owned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("registry redirect refused")
	}
	owned.Transport = approvedDialTransport(client.Transport, allowedPrivateHosts)
	return &HTTPResolver{client: &owned, allowedPrivateHosts: allowedPrivateHosts}
}

// approvedDialTransport returns base's transport with a dialer that
// resolves registry hosts itself, validates the address it is about to
// dial, and connects to that exact address. The request-time destination
// check sees one DNS answer; the connection would see another (DNS
// rebinding), so validation must happen at the dial and the approved
// address must be pinned — otherwise a host that answers the check with
// a public address and the dial with a private one walks the control
// plane into internal services. TLS server name and certificate
// validation keep using the request hostname; only the connection target
// is pinned. Operator-allowlisted private registries dial as declared.
// RoundTrippers that never dial (test stubs) pass through unchanged.
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

// dialApprovedAddress validates addr at every dial and pins the approved
// address: the connection is made to the checked IP directly, so no
// second resolution can swap in an unverified one between check and
// connect. Resolution failures fail closed.
func dialApprovedAddress(ctx context.Context, network, addr string, allowedPrivateHosts []string, next func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if registryHostAllowed(addr, allowedPrivateHosts) || registryHostAllowed(hostnameOf(addr), allowedPrivateHosts) {
		// An operator-declared internal registry: dial it as configured.
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
	for _, candidate := range addrs {
		if prohibitedIP(candidate.IP) {
			continue
		}
		return next(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
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
	if err := ValidateManifestDigest(digest); err != nil {
		return ResolvedImage{}, fmt.Errorf("registry %s returned an invalid manifest digest for %s: %w", host, ref, err)
	}
	return ResolvedImage{Repository: parsed.Repository, ManifestDigest: digest, Ref: parsed.Repository + "@" + digest}, nil
}

func registryScheme(host string) string {
	bare := host
	if h, _, ok := strings.Cut(host, ":"); ok {
		bare = h
	}
	if bare == "localhost" || bare == "127.0.0.1" || bare == "::1" {
		return "http"
	}
	return "https"
}

// registryEndpoint maps a repository host to its OCI Distribution API
// endpoint. Docker Hub's API lives at registry-1.docker.io: docker.io is
// web content and serves no /v2/ routes, so ordinary Docker Hub tags
// must not resolve against it.
func registryEndpoint(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	default:
		return host
	}
}

// manifestURLFor builds the manifest endpoint URL for a normalized
// repository and a tag or digest reference.
func manifestURLFor(repository, reference string) string {
	host, path, _ := strings.Cut(repository, "/")
	endpoint := registryEndpoint(host)
	return registryScheme(endpoint) + "://" + endpoint + "/v2/" + path + "/manifests/" + reference
}

// bearerAuthError carries a parsed Bearer challenge from a 401 response.
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
		// A registry that refuses HEAD still answers GET.
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

// manifestRequest performs one manifest HEAD or GET against the
// registry's canonical endpoint. Redirects are refused by the resolver's
// client (see NewHTTPResolver): a manifest redirect must not hand the
// registry a reachable path to internal services.
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

// anonymousToken fetches an anonymous pull token from a Bearer
// challenge's realm. The realm is validated against the registry that
// issued the challenge first (see tokenRealmURL), and like all registry
// traffic the fetch follows no redirects (see NewHTTPResolver): one
// compromised hop must not walk the control plane toward internal URLs.
func (r *HTTPResolver) anonymousToken(ctx context.Context, challenge bearerChallengeValues, registryHost, repository string) (string, error) {
	tokenURL, err := tokenRealmURL(ctx, challenge.realm, registryHost, r.allowedPrivateHosts)
	if err != nil {
		return "", err
	}
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
	resp, err := r.client.Do(req)
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
// environments. Pinned references pass through the same validation as the
// HTTP resolver; tags without an entry fall back to Fallback when set and
// otherwise report ErrImageNotFound.
type StaticResolver struct {
	Tags     map[string]string
	Fallback func(ref string) (string, bool)
}

// StaticResolverForTest builds a resolver whose fallback pins every tag to
// the sha256 of its normalized form. Test images never leave the process,
// and repeated resolves of one tag converge on one digest.
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
