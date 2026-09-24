package registry

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// repositoryComponentPattern mirrors the Docker distribution name-component
// grammar: lowercase alphanumeric runs joined by single dots, single or
// double underscores, or runs of dashes.
var repositoryComponentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:__|[._]|[-]+)[a-z0-9]+)*$`)

// ParsedReference is a normalized OCI image reference. Repository always
// carries an explicit host (default docker.io, with the library/ prefix
// applied to single-component Docker Hub paths). Exactly one of Tag and
// Digest identifies the image; when the input carries both
// (repository:tag@digest) the digest wins and the tag is dropped.
type ParsedReference struct {
	Repository string
	Tag        string
	Digest     string
}

// Pinned reports whether the reference resolves to an immutable digest.
func (r ParsedReference) Pinned() bool {
	return r.Digest != ""
}

// PinnedRef returns the digest-pinned runtime form repository@digest.
// It is empty when the reference is not pinned.
func (r ParsedReference) PinnedRef() string {
	if r.Digest == "" {
		return ""
	}
	return r.Repository + "@" + r.Digest
}

// Familiar returns the human-readable form: repository:tag, or the pinned
// form when no tag survives.
func (r ParsedReference) Familiar() string {
	if r.Tag != "" {
		return r.Repository + ":" + r.Tag
	}
	return r.PinnedRef()
}

// ParseReference normalizes an image reference the way the Docker/OCI
// tooling does: an absent host becomes docker.io, a bare Docker Hub name
// gains the library/ prefix, a missing tag becomes latest, and whitespace
// around a digest is normalized away. Pinned references must carry a full
// sha256 manifest digest; anything else is rejected so a malformed digest
// can never become runtime identity.
func ParseReference(ref string) (ParsedReference, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return ParsedReference{}, errors.New("image reference is required")
	}
	if len(trimmed) > 512 {
		return ParsedReference{}, errors.New("image reference is too long")
	}

	repository := trimmed
	var digest string
	if at := strings.LastIndexByte(trimmed, '@'); at >= 0 {
		// Whitespace and hex case around the digest are normalized away so
		// a pasted "repo@ sha256:..." or uppercase hex can never smuggle a
		// non-canonical identity into the stored runtime reference.
		repository = strings.TrimSpace(trimmed[:at])
		digest = strings.ToLower(strings.TrimSpace(trimmed[at+1:]))
		if repository == "" {
			return ParsedReference{}, fmt.Errorf("image reference %q has no repository", ref)
		}
		if err := ValidateManifestDigest(digest); err != nil {
			return ParsedReference{}, fmt.Errorf("image reference %q: %w", ref, err)
		}
	}

	var tag string
	tagged := false
	if digest == "" {
		repository, tag, tagged = splitTag(repository)
	} else if sep := strings.LastIndexByte(repository, ':'); sep > strings.LastIndexByte(repository, '/') {
		// repository:tag@digest pins by digest; the tag is informational.
		repository = repository[:sep]
	}

	normalized, err := normalizeRepository(repository)
	if err != nil {
		return ParsedReference{}, fmt.Errorf("image reference %q: %w", ref, err)
	}
	switch {
	case digest != "":
	case !tagged:
		tag = "latest"
	case tag == "":
		return ParsedReference{}, fmt.Errorf("image reference %q: image tag is required after ':'", ref)
	default:
		if err := validateTag(tag); err != nil {
			return ParsedReference{}, fmt.Errorf("image reference %q: %w", ref, err)
		}
	}
	return ParsedReference{Repository: normalized, Tag: tag, Digest: digest}, nil
}

// SplitPinnedReference splits a digest-pinned runtime reference into
// repository and manifest digest without applying registry defaults. It is
// the structural parse for values the platform already pinned (builder
// completions, stored artifacts); user input goes through ParseReference.
func SplitPinnedReference(ref string) (repository, digest string, err error) {
	trimmed := strings.TrimSpace(ref)
	at := strings.LastIndexByte(trimmed, '@')
	if at <= 0 || at == len(trimmed)-1 {
		return "", "", fmt.Errorf("image reference %q is not digest-pinned", ref)
	}
	repository, digest = strings.TrimSpace(trimmed[:at]), strings.ToLower(strings.TrimSpace(trimmed[at+1:]))
	if repository == "" || digest == "" {
		return "", "", fmt.Errorf("image reference %q is not digest-pinned", ref)
	}
	if err := ValidateManifestDigest(digest); err != nil {
		return "", "", fmt.Errorf("image reference %q: %w", ref, err)
	}
	return repository, digest, nil
}

// IsDigestPinned reports whether ref is a structurally valid digest-pinned
// reference with a full sha256 digest.
func IsDigestPinned(ref string) bool {
	_, _, err := SplitPinnedReference(ref)
	return err == nil
}

// ValidateManifestDigest requires a full sha256 digest in canonical
// lowercase hex: stops a truncated or mistyped digest from pinning a
// deployment to an image that can never be pulled (containerd rejects
// non-canonical digests), and keeps stored runtime identities canonical.
func ValidateManifestDigest(digest string) error {
	const prefix = "sha256:"
	hexPart, ok := strings.CutPrefix(strings.TrimSpace(digest), prefix)
	if !ok {
		return errors.New("digest must start with sha256:")
	}
	if len(hexPart) != 64 {
		return errors.New("digest must contain a full 64-character sha256")
	}
	for _, ch := range hexPart {
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			continue
		}
		return errors.New("sha256 digest must be lowercase hexadecimal")
	}
	return nil
}

func splitTag(repository string) (string, string, bool) {
	if sep := strings.LastIndexByte(repository, ':'); sep > strings.LastIndexByte(repository, '/') {
		return repository[:sep], repository[sep+1:], true
	}
	return repository, "", false
}

func normalizeRepository(repository string) (string, error) {
	if repository == "" {
		return "", errors.New("repository is required")
	}
	host, path, _ := strings.Cut(repository, "/")
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") && !strings.EqualFold(host, "localhost") {
		// No explicit host: the whole input is a Docker Hub path.
		path = repository
		host = "docker.io"
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", errors.New("repository host is required")
	}
	if err := validateRegistryHostSyntax(host); err != nil {
		return "", err
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return "", errors.New("repository path is required")
	}
	if host == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if len(host)+1+len(path) > 255 {
		return "", errors.New("repository is too long")
	}
	for _, component := range strings.Split(path, "/") {
		if err := validateRepositoryComponent(component); err != nil {
			return "", err
		}
	}
	return host + "/" + path, nil
}

func validateRepositoryComponent(component string) error {
	if component == "" || len(component) > 128 {
		return errors.New("repository path has an empty or oversized component")
	}
	if !repositoryComponentPattern.MatchString(component) {
		return errors.New("repository path must be lowercase alphanumeric with . _ - separators")
	}
	return nil
}

// registryHostNamePattern is RFC 1123 host syntax: labels of alphanumerics
// and inner hyphens, dot-separated.
var registryHostNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

// validateRegistryHostSyntax rejects hosts no registry can be pulled from
// before a reference is stored. Digest-pinned inputs skip registry I/O at
// resolution, so without this an unpullable host — an invalid name or
// port — is stored and only fails on the agent at deploy time.
func validateRegistryHostSyntax(host string) error {
	name := host
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 || net.ParseIP(host[1:end]) == nil {
			return fmt.Errorf("registry host %q is not a valid IP literal", host)
		}
		if rest := host[end+1:]; rest != "" {
			return validateRegistryHostPort(rest)
		}
		return nil
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		if err := validateRegistryHostPort(":" + port); err != nil {
			return err
		}
		name = h
	}
	if net.ParseIP(name) != nil {
		return nil
	}
	if len(name) > 253 || !registryHostNamePattern.MatchString(name) {
		return fmt.Errorf("registry host %q is not a valid host name", host)
	}
	return nil
}

func validateRegistryHostPort(port string) error {
	value := strings.TrimPrefix(port, ":")
	if value == "" {
		return errors.New("registry host has an empty port")
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fmt.Errorf("registry host port %q is not numeric", value)
		}
	}
	if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("registry host port %q is out of range", value)
	}
	return nil
}

func validateTag(tag string) error {
	if tag == "" || len(tag) > 128 {
		return errors.New("image tag is required and must be at most 128 characters")
	}
	for _, ch := range tag {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '.', ch == '-':
		default:
			return fmt.Errorf("image tag %q uses an invalid character", tag)
		}
	}
	return nil
}
