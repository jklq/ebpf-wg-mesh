package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVirtCustomizeArgsEnablesServicesByLink(t *testing.T) {
	args, err := virtCustomizeArgs(localImageRecipe("deadbeef", "http://archive.ubuntu.com/ubuntu"), "dest.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	if strings.Contains(joined, "systemctl enable") {
		t.Fatal("virt-customize still uses systemctl enable")
	}
	if !strings.Contains(joined, "--link") || !strings.Contains(joined, "containerd.service") {
		t.Fatalf("missing containerd wants link: %v", args)
	}
	if !strings.Contains(joined, "/var/lib/dbus/machine-id") {
		t.Fatal("dbus machine-id is not reset")
	}
	if _, err := virtCustomizeArgs(localImageRecipe("deadbeef", "http://x/ubuntu;id"), "dest.qcow2"); err == nil {
		t.Fatal("expected apt-mirror rejection in virt-customize args")
	}
}

func TestImageRecipeKeyTracksInputs(t *testing.T) {
	base := localImageRecipe("abcdef", "")
	if base.key() != localImageRecipe("ABCDEF", "").key() {
		t.Fatal("key should normalize the base digest case")
	}
	changed := base
	changed.Packages = append(slices.Clone(base.Packages), "extra")
	cases := map[string]imageRecipe{
		"base digest": {BaseSHA: "123456", Packages: base.Packages, Services: base.Services, Builder: base.Builder},
		"packages":    changed,
		"services":    {BaseSHA: base.BaseSHA, Packages: base.Packages, Services: []string{"containerd", "other"}, Builder: base.Builder},
		"apt mirror":  {BaseSHA: base.BaseSHA, Packages: base.Packages, Services: base.Services, AptMirror: "http://mirror.example/ubuntu", Builder: base.Builder},
		"builder":     {BaseSHA: base.BaseSHA, Packages: base.Packages, Services: base.Services, Builder: base.Builder + 1},
	}
	for name, recipe := range cases {
		if recipe.key() == base.key() {
			t.Fatalf("%s change did not change the key", name)
		}
	}
}

type fakeImageBuilder struct {
	calls  int
	recipe imageRecipe
	err    error
}

type concurrentImageBuilder struct {
	mu    sync.Mutex
	dests []string
}

func (b *concurrentImageBuilder) Build(_ context.Context, _ imageRecipe, _, dest string) error {
	b.mu.Lock()
	b.dests = append(b.dests, dest)
	b.mu.Unlock()
	return os.WriteFile(dest, []byte("prepared image"), 0o644)
}

func TestImageManagerConcurrentEnsureBuildsOnce(t *testing.T) {
	builder := &concurrentImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })
	recipe := localImageRecipe("deadbeef", "")
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := manager.ensure(context.Background(), "base.qcow2", recipe)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	builder.mu.Lock()
	defer builder.mu.Unlock()
	if len(builder.dests) != 1 {
		t.Fatalf("concurrent build destinations = %v, want one build", builder.dests)
	}
}

func (f *fakeImageBuilder) Build(_ context.Context, recipe imageRecipe, _, dest string) error {
	f.calls++
	f.recipe = recipe
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(dest, []byte("prepared image"), 0o644)
}

func newTestImageManager(t *testing.T, builder imageBuilder, validate func(context.Context, string) error) *imageManager {
	t.Helper()
	return &imageManager{
		cacheDir: t.TempDir(),
		builder:  builder,
		validate: validate,
		now:      func() time.Time { return time.Unix(0, 0).UTC() },
	}
}

func TestImageManagerBuildsThenReusesValidCache(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })

	path, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", ""))
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if builder.calls != 1 {
		t.Fatalf("builder calls = %d, want 1", builder.calls)
	}
	if !strings.Contains(filepath.Base(path), localPreparedImagePrefix) {
		t.Fatalf("prepared path %q lacks prefix", path)
	}
	if _, err := os.Stat(path + ".json"); err != nil {
		t.Fatalf("missing sidecar: %v", err)
	}

	reused, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", ""))
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if reused != path {
		t.Fatalf("reused %q want %q", reused, path)
	}
	if builder.calls != 1 {
		t.Fatalf("builder calls = %d, want 1 (cache should be reused)", builder.calls)
	}
}

func TestImageManagerRebuildsWhenRecipeChanges(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })

	first, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", ""))
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("cafebabe", ""))
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if first == second {
		t.Fatal("changed base digest reused the same image")
	}
	if builder.calls != 2 {
		t.Fatalf("builder calls = %d, want 2", builder.calls)
	}
}

func TestImageManagerRebuildsWhenValidationFails(t *testing.T) {
	builder := &fakeImageBuilder{}
	valid := false
	manager := newTestImageManager(t, builder, func(context.Context, string) error {
		if valid {
			return nil
		}
		return os.ErrInvalid
	})

	if _, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", "")); err == nil {
		t.Fatal("expected validation failure to fail the build")
	}
	if builder.calls != 1 {
		t.Fatalf("builder calls = %d, want 1", builder.calls)
	}
	if _, err := os.Stat(filepath.Join(manager.cacheDir, localPreparedImagePrefix+localImageRecipe("deadbeef", "").key()+".qcow2")); !os.IsNotExist(err) {
		t.Fatal("invalid image was published to the cache")
	}

	valid = true
	if _, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", "")); err != nil {
		t.Fatalf("ensure after validation fixed: %v", err)
	}
	if builder.calls != 2 {
		t.Fatalf("builder calls = %d, want 2", builder.calls)
	}
}

func TestImageManagerRebuildsOnMissingSidecar(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })
	recipe := localImageRecipe("deadbeef", "")
	path, err := manager.ensure(context.Background(), "base.qcow2", recipe)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := os.Remove(path + ".json"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ensure(context.Background(), "base.qcow2", recipe); err != nil {
		t.Fatalf("ensure after missing sidecar: %v", err)
	}
	if builder.calls != 2 {
		t.Fatalf("builder calls = %d, want 2", builder.calls)
	}
}

func TestImageManagerRebuildsOnCorruptSidecar(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })
	recipe := localImageRecipe("deadbeef", "")
	path, err := manager.ensure(context.Background(), "base.qcow2", recipe)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := os.WriteFile(path+".json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ensure(context.Background(), "base.qcow2", recipe); err != nil {
		t.Fatalf("ensure after corrupt sidecar: %v", err)
	}
	if builder.calls != 2 {
		t.Fatalf("builder calls = %d, want 2", builder.calls)
	}
}

func TestImageManagerRebuildsOnKeyMismatch(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })
	recipe := localImageRecipe("deadbeef", "")
	path, err := manager.ensure(context.Background(), "base.qcow2", recipe)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	meta, err := readImageMeta(path + ".json")
	if err != nil {
		t.Fatal(err)
	}
	meta.Key = "not-the-recipe-key"
	if err := writeImageMeta(path+".json", meta); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ensure(context.Background(), "base.qcow2", recipe); err != nil {
		t.Fatalf("ensure after key mismatch: %v", err)
	}
	if builder.calls != 2 {
		t.Fatalf("builder calls = %d, want 2", builder.calls)
	}
}

func TestImageManagerAbortsOnHardCacheError(t *testing.T) {
	builder := &fakeImageBuilder{}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })
	recipe := localImageRecipe("deadbeef", "")
	if _, err := manager.ensure(context.Background(), "base.qcow2", recipe); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not hide files from root")
	}
	if err := os.Chmod(manager.cacheDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manager.cacheDir, 0o755) })
	if _, err := manager.ensure(context.Background(), "base.qcow2", recipe); err == nil {
		t.Fatal("expected hard cache error to abort rather than rebuild")
	}
	if builder.calls != 1 {
		t.Fatalf("builder calls = %d, want 1 (must not rebuild over a permission error)", builder.calls)
	}
}

func TestCacheRebuildable(t *testing.T) {
	if !cacheRebuildable(os.ErrNotExist) {
		t.Fatal("missing file should rebuild")
	}
	if !cacheRebuildable(errors.New("image is empty")) {
		t.Fatal("empty image should rebuild")
	}
	if !cacheRebuildable(fmt.Errorf("recipe changed: cached a want b")) {
		t.Fatal("key mismatch should rebuild")
	}
	if cacheRebuildable(&os.PathError{Op: "stat", Path: "x", Err: os.ErrPermission}) {
		t.Fatal("permission error must not rebuild")
	}
}

func TestEnsureLocalBaseImageExplicitSkipsChecksumFetch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.img")
	content := []byte("explicit-base-image-contents")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("explicit base image must not contact upstream: %s", r.URL)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	origDir := localBaseImageDir
	localBaseImageDir = server.URL + "/"
	t.Cleanup(func() { localBaseImageDir = origDir })

	got, err := ensureLocalBaseImage(context.Background(), path, false)
	if err != nil {
		t.Fatalf("ensure explicit: %v", err)
	}
	want, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %s want %s", got, want)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(content) {
		t.Fatal("explicit base image was overwritten")
	}
}

func TestEnsureLocalBaseImageCleansPartOnDownloadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, localBaseImageName)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHA256SUMS"):
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("a", 64), localBaseImageName)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	origDir := localBaseImageDir
	localBaseImageDir = server.URL + "/"
	t.Cleanup(func() { localBaseImageDir = origDir })

	if _, err := ensureLocalBaseImage(context.Background(), path, true); err == nil {
		t.Fatal("expected download failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".part") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed download published a base image")
	}
}

func TestImageManagerDoesNotPublishFailedBuild(t *testing.T) {
	builder := &fakeImageBuilder{err: os.ErrPermission}
	manager := newTestImageManager(t, builder, func(context.Context, string) error { return nil })

	if _, err := manager.ensure(context.Background(), "base.qcow2", localImageRecipe("deadbeef", "")); err == nil {
		t.Fatal("expected builder error")
	}
	entries, err := os.ReadDir(manager.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".prepared-image.lock" {
			t.Fatalf("failed build left artifact: %s", entry.Name())
		}
	}
}

func TestLocalImagePackagesCoverCloudInitTemplates(t *testing.T) {
	for _, name := range []string{"agent.yaml.tftpl", "controlplane.yaml.tftpl"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "infra", "test-vm", "cloud-init", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, pkg := range parseCloudInitPackages(t, string(data)) {
			if !slices.Contains(localImagePackages, pkg) {
				t.Errorf("%s installs %q, which the local prepared image recipe omits", name, pkg)
			}
		}
	}
}

func parseCloudInitPackages(t *testing.T, content string) []string {
	t.Helper()
	var packages []string
	inPackages := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.TrimSpace(line) == "packages:":
			inPackages = true
		case inPackages && strings.HasPrefix(line, "  - "):
			packages = append(packages, strings.TrimSpace(strings.TrimPrefix(line, "  - ")))
		case inPackages && line != "" && !strings.HasPrefix(line, " "):
			inPackages = false
		}
	}
	if len(packages) == 0 {
		t.Fatal("no packages parsed from cloud-init template")
	}
	return packages
}
