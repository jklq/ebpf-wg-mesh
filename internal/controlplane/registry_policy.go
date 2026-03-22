package controlplane

import (
	"fmt"
	"strings"

	"ebof-wg-mesh/internal/config"
)

type RegistryPolicy struct {
	host            string
	namespacePrefix string
	username        string
	password        string
}

func NewRegistryPolicy(cfg config.RegistryConfig) *RegistryPolicy {
	return &RegistryPolicy{
		host:            strings.TrimSpace(cfg.Host),
		namespacePrefix: trimRegistryPath(cfg.NamespacePrefix),
		username:        cfg.Username,
		password:        cfg.Password,
	}
}

func (p *RegistryPolicy) Enabled() bool {
	return p != nil && p.host != ""
}

func (p *RegistryPolicy) PushRef(projectID, serviceID, commitSHA string) string {
	if !p.Enabled() {
		return ""
	}
	segments := []string{p.host}
	if p.namespacePrefix != "" {
		segments = append(segments, p.namespacePrefix)
	}
	segments = append(segments, sanitizeRefSegment(projectID), sanitizeRefSegment(serviceID))
	return strings.Join(segments, "/") + ":git-" + sanitizeTag(commitSHA)
}

func (p *RegistryPolicy) RuntimeDigestRef(pushRef, digest string) string {
	if pushRef == "" || digest == "" {
		return ""
	}
	base, _, _ := strings.Cut(pushRef, ":")
	return base + "@" + digest
}

func (p *RegistryPolicy) Credentials() (string, string) {
	if p == nil {
		return "", ""
	}
	return p.username, p.password
}

func (p *RegistryPolicy) CredentialsForPushRef(pushRef string) (string, string, error) {
	_ = pushRef
	if p == nil {
		return "", "", nil
	}
	return p.username, p.password, nil
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
	return strings.Trim(strings.ReplaceAll(b.String(), "--", "-"), "-")
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
