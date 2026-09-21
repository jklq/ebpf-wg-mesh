package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestSecretFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o600)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func TestResolveHMACSecret(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "secret")
	if err := writeTestSecretFile(t, file, "file-supplied-secret-at-least-32-bytes!!"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		scopes    []string
		secret    string
		file      string
		want      string
		wantError string
	}{
		{name: "empty generates", scopes: []string{"user-assertion"}},
		{name: "flag secret", scopes: []string{"user-assertion"}, secret: "flag-supplied-secret-at-least-32-bytes!!", want: "flag-supplied-secret-at-least-32-bytes!!"},
		{name: "file secret", scopes: []string{"dashboard-session"}, file: file, want: "file-supplied-secret-at-least-32-bytes!!"},
		{name: "flag and file conflict", scopes: []string{"user-assertion"}, secret: "x", file: file, wantError: "only one of hmac secret"},
		{name: "multi-scope secret rejected", scopes: []string{"user-assertion", "dashboard-session"}, secret: "x", wantError: "exactly one --scope"},
		{name: "ecdsa scope rejected", scopes: []string{"internal-ca"}, secret: "x", wantError: "HMAC scopes only"},
		{name: "unknown scope", scopes: []string{"bogus"}, secret: "x", wantError: "unknown signing scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := tc.file
			got, err := resolveHMACSecret(tc.scopes, tc.secret, file)
			if tc.wantError != "" {
				if err == nil || !containsFold(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHMACSecret: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("secret = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecretForScope(t *testing.T) {
	t.Parallel()

	if got := secretForScope("user-assertion", []byte("s")); string(got) != "s" {
		t.Fatalf("hmac scope kept %q", got)
	}
	if got := secretForScope("internal-ca", []byte("s")); got != nil {
		t.Fatalf("ecdsa scope kept %q", got)
	}
	if got := secretForScope("user-assertion", nil); got != nil {
		t.Fatalf("empty secret kept %q", got)
	}
}

func TestScopeListFlag(t *testing.T) {
	t.Parallel()

	var scopes scopeListFlag
	if err := scopes.Set("internal-ca"); err != nil {
		t.Fatal(err)
	}
	if err := scopes.Set("registry"); err != nil {
		t.Fatal(err)
	}
	if scopes.String() != "internal-ca,registry" {
		t.Fatalf("scopes = %q", scopes.String())
	}
}
