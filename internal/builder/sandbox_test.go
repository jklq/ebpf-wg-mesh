package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sandboxMountsForTest(t *testing.T) (root string, mounts []SandboxMount) {
	t.Helper()
	root = t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	resolv := filepath.Join(root, "resolv.conf")
	hosts := filepath.Join(root, "hosts")
	for _, path := range []string{resolv, hosts} {
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, []SandboxMount{
		{Source: root, Dest: "/build"},
		{Source: repo, Dest: "/build/repo", ReadOnly: true},
		{Source: resolv, Dest: "/etc/resolv.conf", ReadOnly: true},
		{Source: hosts, Dest: "/etc/hosts", ReadOnly: true},
	}
}

func TestValidateSandboxMounts(t *testing.T) {
	t.Parallel()

	root, good := sandboxMountsForTest(t)
	if err := validateSandboxMounts(good); err != nil {
		t.Fatalf("valid mounts must pass: %v", err)
	}
	// Order must not matter: the nested read-only snapshot sorts
	// after the writable root at OCI conversion time.
	reversed := append([]SandboxMount(nil), good...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if err := validateSandboxMounts(reversed); err != nil {
		t.Fatalf("reordered mounts must pass: %v", err)
	}

	cases := map[string][]SandboxMount{
		"empty": nil,
		"missing build root": {
			{Source: root, Dest: "/build/repo", ReadOnly: true},
		},
		"destination outside build root": {
			{Source: root, Dest: "/build"},
			{Source: root, Dest: "/etc/passwd"},
		},
		"relative source": {
			{Source: root, Dest: "/build"},
			{Source: "relative/path", Dest: "/build/extra"},
		},
		"missing source": {
			{Source: root, Dest: "/build"},
			{Source: filepath.Join(root, "does-not-exist"), Dest: "/build/extra"},
		},
		"duplicate destination": {
			{Source: root, Dest: "/build"},
			{Source: root, Dest: "/build"},
		},
		"root filesystem": {
			{Source: root, Dest: "/"},
		},
	}
	for name, mounts := range cases {
		if err := validateSandboxMounts(mounts); err == nil {
			t.Fatalf("%s: expected mount rejection", name)
		}
	}

	// Host runtime paths must never be mountable destinations: prove
	// the destination rule rejects them with an existing source.
	if err := validateSandboxMounts([]SandboxMount{
		{Source: root, Dest: "/build"},
		{Source: root, Dest: "/run/sandbox-escape"},
	}); err == nil || !strings.Contains(err.Error(), "outside the build root") {
		t.Fatalf("expected outside-build-root rejection, got %v", err)
	}
}

func TestGuestWorkspacePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for host, want := range map[string]string{
		root:                        "/build",
		filepath.Join(root, "repo"): "/build/repo",
		filepath.Join(root, "s", "bk.sock"): "/build/s/bk.sock",
	} {
		got, err := guestWorkspacePath(root, host)
		if err != nil || got != want {
			t.Fatalf("guestWorkspacePath(%q) = %q, %v; want %q", host, got, err, want)
		}
	}
	for _, host := range []string{
		filepath.Join(root, "..", "escape"),
		filepath.Dir(root),
		"/etc/hostname",
	} {
		if got, err := guestWorkspacePath(root, host); err == nil {
			t.Fatalf("guestWorkspacePath(%q) = %q, want rejection", host, got)
		}
	}
}

func TestSandboxContainerID(t *testing.T) {
	t.Parallel()

	id, err := sandboxContainerID("build-1", "plan")
	if err != nil || id != "build-build-1-plan" {
		t.Fatalf("unexpected container id %q, %v", id, err)
	}
	id, err = sandboxContainerID("build/1:evil", "net step")
	if err != nil {
		t.Fatalf("sanitizable id must pass: %v", err)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			t.Fatalf("container id %q contains %q", id, r)
		}
	}
	if _, err := sandboxContainerID("", "plan"); err == nil {
		t.Fatal("expected empty build id to fail")
	}
	if _, err := sandboxContainerID("build-1", ""); err == nil {
		t.Fatal("expected empty step to fail")
	}
}

func TestDeniedRoutePrefixes(t *testing.T) {
	t.Parallel()

	policy := DefaultRestrictedNetworkPolicy()
	prefixes, err := deniedRoutePrefixes(policy)
	if err != nil {
		t.Fatalf("default policy must parse: %v", err)
	}
	if len(prefixes) != len(policy.DeniedCIDRs) {
		t.Fatalf("expected %d prefixes, got %d", len(policy.DeniedCIDRs), len(prefixes))
	}
	policy.DeniedCIDRs = append(policy.DeniedCIDRs, "not-a-cidr")
	if _, err := deniedRoutePrefixes(policy); err == nil {
		t.Fatal("expected invalid CIDR to fail closed")
	}
}

func TestRenderSandboxResolvConf(t *testing.T) {
	t.Parallel()

	explicit, err := renderSandboxResolvConf(nil, []string{"10.0.0.53", "2001:db8::53"}, true)
	if err != nil {
		t.Fatalf("explicit nameservers: %v", err)
	}
	if !strings.Contains(string(explicit), "nameserver 10.0.0.53") || !strings.Contains(string(explicit), "nameserver 2001:db8::53") {
		t.Fatalf("explicit resolvers missing: %q", explicit)
	}
	if _, err := renderSandboxResolvConf(nil, []string{"not-an-ip"}, true); err == nil {
		t.Fatal("expected invalid nameserver to fail")
	}

	host := []byte("# comment\nnameserver 127.0.0.53\nnameserver 10.0.0.53\nnameserver ::1\nsearch example.test\n")
	inherited, err := renderSandboxResolvConf(host, nil, true)
	if err != nil {
		t.Fatalf("inherit resolvers: %v", err)
	}
	if !strings.Contains(string(inherited), "nameserver 10.0.0.53") {
		t.Fatalf("expected host resolver to survive: %q", inherited)
	}
	if strings.Contains(string(inherited), "127.0.0.53") || strings.Contains(string(inherited), "::1") {
		t.Fatalf("loopback resolvers must be dropped: %q", inherited)
	}

	loopbackOnly := []byte("nameserver 127.0.0.53\n")
	if _, err := renderSandboxResolvConf(loopbackOnly, nil, true); err == nil || !strings.Contains(err.Error(), "builder.sandbox.nameservers") {
		t.Fatalf("expected fail-closed nameserver error, got %v", err)
	}
	denied, err := renderSandboxResolvConf(loopbackOnly, nil, false)
	if err != nil {
		t.Fatalf("deny-egress needs no resolvers: %v", err)
	}
	if strings.Contains(string(denied), "nameserver") {
		t.Fatalf("deny-egress resolv.conf must be empty: %q", denied)
	}
}

func TestRenderSandboxHostsFile(t *testing.T) {
	t.Parallel()

	hosts := string(renderSandboxHostsFile())
	for _, want := range []string{"127.0.0.1 localhost", "::1 localhost", "127.0.0.1 build"} {
		if !strings.Contains(hosts, want) {
			t.Fatalf("hosts file %q missing %q", hosts, want)
		}
	}
}

func TestSelectCNIConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mesh := `{"cniVersion":"1.0.0","name":"mesh-cni","plugins":[{"type":"bridge"}]}`
	build := `{"cniVersion":"1.0.0","name":"build-sandbox","plugins":[{"type":"bridge"}]}`
	// The mesh config sorts first: selection must be by exact name,
	// never by directory order.
	if err := os.WriteFile(filepath.Join(dir, "10-mesh-cni.conflist"), []byte(mesh), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "90-build.conflist"), []byte(build), 0o644); err != nil {
		t.Fatal(err)
	}
	path, isList, err := selectCNIConfig(dir, "build-sandbox")
	if err != nil || !isList || !strings.HasSuffix(path, "90-build.conflist") {
		t.Fatalf("selectCNIConfig = %q, %v, %v", path, isList, err)
	}
	if _, _, err := selectCNIConfig(dir, "mesh-cni"); err != nil {
		t.Fatalf("mesh selection must work when asked: %v", err)
	}
	if _, _, err := selectCNIConfig(dir, "does-not-exist"); err == nil {
		t.Fatal("expected missing network to fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "99-bad.conflist"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectCNIConfig(dir, "build-sandbox"); err == nil {
		t.Fatal("expected malformed config to fail closed")
	}

	single := t.TempDir()
	if err := os.WriteFile(filepath.Join(single, "build.conf"), []byte(`{"cniVersion":"1.0.0","name":"build-sandbox","type":"bridge"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path, isList, err = selectCNIConfig(single, "build-sandbox")
	if err != nil || isList {
		t.Fatalf("single .conf selection = %q, isList=%v, %v", path, isList, err)
	}
	if _, _, err := selectCNIConfig(filepath.Join(dir, "does-not-exist"), "build-sandbox"); err == nil {
		t.Fatal("expected missing conf dir to fail")
	}
}
