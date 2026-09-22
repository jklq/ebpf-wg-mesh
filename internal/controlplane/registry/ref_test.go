package registry

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("a1", 32)
	tests := []struct {
		name       string
		input      string
		repository string
		tag        string
		digest     string
		wantErr    bool
	}{
		{name: "bare name", input: "busybox", repository: "docker.io/library/busybox", tag: "latest"},
		{name: "name and tag", input: "busybox:1.36", repository: "docker.io/library/busybox", tag: "1.36"},
		{name: "namespaced", input: "ghcr.io/demo/echo:latest", repository: "ghcr.io/demo/echo", tag: "latest"},
		{name: "registry port", input: "registry.example.test:5000/mesh/app:git-deadbeef", repository: "registry.example.test:5000/mesh/app", tag: "git-deadbeef"},
		{name: "pinned", input: "example.test/web@" + digest, repository: "example.test/web", digest: digest},
		{name: "tag and digest prefers digest", input: "example.test/web:v1@" + digest, repository: "example.test/web", digest: digest},
		{name: "whitespace trimmed", input: "  nginx:1.27  ", repository: "docker.io/library/nginx", tag: "1.27"},
		{name: "empty", input: "", wantErr: true},
		{name: "missing repository", input: "@" + digest, wantErr: true},
		{name: "short digest", input: "example.test/web@sha256:abc", wantErr: true},
		{name: "wrong algorithm", input: "example.test/web@sha512:" + strings.Repeat("a1", 32), wantErr: true},
		{name: "uppercase repository", input: "Example.Test/Web:1", wantErr: true},
		{name: "bad tag", input: "busybox:1 36", wantErr: true},
		{name: "empty tag", input: "busybox:", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseReference(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseReference(%q) succeeded, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tc.input, err)
			}
			if parsed.Repository != tc.repository || parsed.Tag != tc.tag || parsed.Digest != tc.digest {
				t.Fatalf("ParseReference(%q) = %+v, want repository=%q tag=%q digest=%q",
					tc.input, parsed, tc.repository, tc.tag, tc.digest)
			}
			if tc.digest != "" && (parsed.PinnedRef() != tc.repository+"@"+tc.digest || !parsed.Pinned()) {
				t.Fatalf("ParseReference(%q) pinned form = %q", tc.input, parsed.PinnedRef())
			}
		})
	}
}

func TestIsDigestPinned(t *testing.T) {
	t.Parallel()

	pinned := "registry.example.test/platform/web@sha256:" + strings.Repeat("ab", 32)
	if !IsDigestPinned(pinned) {
		t.Fatalf("IsDigestPinned(%q) = false, want true", pinned)
	}
	for _, ref := range []string{"", "nginx:latest", "example.test/web@sha256:deadbeef", "example.test/web@sha256:" + strings.Repeat("zz", 32)} {
		if IsDigestPinned(ref) {
			t.Fatalf("IsDigestPinned(%q) = true, want false", ref)
		}
	}
}
