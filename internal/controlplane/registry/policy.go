package registry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
)

var registryPushActions = []string{"pull", "push"}

type credentialMinter interface {
	MintCredential(ctx context.Context, subject, repository string, actions []string, expiresAt *time.Time) (string, string, error)
}

type Policy struct {
	host              string
	namespacePrefix   string
	credentialTTL     time.Duration
	pullCredentialTTL time.Duration
	minter            credentialMinter
	now               func() time.Time
}

func NewPolicy(cfg config.RegistryConfig, minter credentialMinter) *Policy {
	pullTTL := time.Duration(cfg.PullCredentialTTLSeconds) * time.Second
	if pullTTL <= 0 {
		pullTTL = 48 * time.Hour
	}
	return &Policy{
		host:              strings.TrimSpace(cfg.Host),
		namespacePrefix:   trimRegistryPath(cfg.NamespacePrefix),
		credentialTTL:     time.Duration(cfg.CredentialTTLSeconds) * time.Second,
		pullCredentialTTL: pullTTL,
		minter:            minter,
		now:               time.Now,
	}
}

func (p *Policy) Enabled() bool {
	return p != nil && p.host != "" && p.credentialTTL > 0
}

func (p *Policy) PushRef(projectID, environmentID, buildID, serviceID, commitSHA string) string {
	if !p.Enabled() {
		return ""
	}
	segments := []string{p.host}
	if p.namespacePrefix != "" {
		segments = append(segments, p.namespacePrefix)
	}
	segments = append(segments, sanitizeRefSegment(projectID), sanitizeRefSegment(environmentID), sanitizeRefSegment(buildID), sanitizeRefSegment(serviceID))
	return strings.Join(segments, "/") + ":git-" + sanitizeTag(commitSHA)
}

func (p *Policy) RuntimeDigestRef(pushRef, digest string) string {
	if pushRef == "" || digest == "" {
		return ""
	}
	base := pushRef
	if tagSeparator := strings.LastIndexByte(pushRef, ':'); tagSeparator > strings.LastIndexByte(pushRef, '/') {
		base = pushRef[:tagSeparator]
	}
	return base + "@" + digest
}

func (p *Policy) CredentialsForBuild(ctx context.Context, projectID, buildID, pushRef string) (string, string, error) {
	if !p.Enabled() || p.minter == nil {
		return "", "", fmt.Errorf("embedded registry auth is not configured")
	}
	repository, err := p.repositoryForReference(pushRef)
	if err != nil {
		return "", "", err
	}
	expectedPrefix := ""
	if p.namespacePrefix != "" {
		expectedPrefix = p.namespacePrefix + "/"
	}
	expectedPrefix += sanitizeRefSegment(projectID) + "/"
	remainder := strings.TrimPrefix(repository, expectedPrefix)
	segments := strings.Split(remainder, "/")
	if remainder == repository || len(segments) != 3 || segments[0] == "" || segments[1] != sanitizeRefSegment(buildID) || segments[2] == "" {
		return "", "", fmt.Errorf("assigned repository does not match project %q and build %q", projectID, buildID)
	}
	expiresAt := p.now().UTC().Add(p.credentialTTL)
	return p.minter.MintCredential(ctx, "build-"+buildID, repository, registryPushActions, &expiresAt)
}

func (p *Policy) CredentialsForPull(ctx context.Context, subject, environmentID, serviceID, imageRef string) (string, string, error) {
	if !p.Enabled() || p.minter == nil {
		return "", "", nil
	}
	if !strings.HasPrefix(imageRef, p.host+"/") {
		return "", "", nil
	}
	repository, err := p.repositoryForReference(imageRef)
	if err != nil {
		return "", "", err
	}
	if p.namespacePrefix != "" && !strings.HasPrefix(repository, p.namespacePrefix+"/") {
		return "", "", fmt.Errorf("image reference is outside platform namespace %q", p.namespacePrefix)
	}
	path := repository
	if p.namespacePrefix != "" {
		path = strings.TrimPrefix(path, p.namespacePrefix+"/")
	}
	segments := strings.Split(path, "/")
	if len(segments) != 4 || segments[0] == "" || segments[1] != sanitizeRefSegment(environmentID) || segments[2] == "" || segments[3] != sanitizeRefSegment(serviceID) {
		return "", "", fmt.Errorf("image repository does not match environment %q and service %q", environmentID, serviceID)
	}
	// Pull capabilities expire so registry rotation can retire: agents
	// receive fresh credentials on every Sync stream, and sessions rotate
	// at least every client-certificate lifetime, so the pull TTL must
	// exceed it (enforced in config validation).
	expiresAt := p.now().UTC().Add(p.pullCredentialTTL)
	return p.minter.MintCredential(ctx, "pull-"+subject, repository, []string{"pull"}, &expiresAt)
}

func (p *Policy) repositoryForReference(ref string) (string, error) {
	prefix := p.host + "/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("image reference is outside registry %q", p.host)
	}
	repositoryAndVersion := strings.TrimPrefix(ref, prefix)
	separator := strings.LastIndexByte(repositoryAndVersion, '@')
	if separator < 0 {
		separator = strings.LastIndexByte(repositoryAndVersion, ':')
		if separator <= strings.LastIndexByte(repositoryAndVersion, '/') {
			return "", fmt.Errorf("image reference has no tag or digest")
		}
	}
	repository := repositoryAndVersion[:separator]
	if repository == "" {
		return "", fmt.Errorf("image reference has no repository")
	}
	return repository, nil
}

func trimRegistryPath(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "/")
}

func sanitizeRefSegment(raw string) string {
	clean := strings.ToLower(strings.TrimSpace(raw))
	if clean == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, ch := range clean {
		switch {
		case ch >= 'a' && ch <= 'z':
			b.WriteRune(ch)
		case ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		case ch == '-' || ch == '_' || ch == '.':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	segment := strings.Trim(strings.ReplaceAll(b.String(), "--", "-"), "-")
	if segment == "" {
		return "unknown"
	}
	return segment
}

func sanitizeTag(commitSHA string) string {
	tag := strings.ToLower(strings.TrimSpace(commitSHA))
	if tag == "" {
		return "unknown"
	}
	if len(tag) > 16 {
		tag = tag[:16]
	}
	for _, ch := range tag {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return fmt.Sprintf("%x", tag)
		}
	}
	return tag
}
