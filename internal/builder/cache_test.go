package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cacheSpec(t *testing.T, scope string, maxBytes int64) ExecutionSpec {
	t.Helper()
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.CacheScope = scope
	spec.Cache = CachePolicy{Mode: CacheModeService, Key: ServiceCacheKey(scope, spec.Recipe, spec.Railpack.FrontendImage), MaxBytes: maxBytes}
	return spec
}

func cacheExport(t *testing.T, bytes int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, bytes), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestServiceCacheReusesSourceChangesAndIsolatesServices(t *testing.T) {
	spec := cacheSpec(t, "project/environment/service", 1000)
	workDir := t.TempDir()
	dir, err := resolveExecutionCacheDir(workDir, spec)
	if err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.SnapshotDigest = "sha256:changed"
	if got, err := resolveExecutionCacheDir(workDir, changed); err != nil || got != dir {
		t.Fatalf("source change lost layer reuse: %s, %v", got, err)
	}
	changed.CacheScope = "another/environment/service"
	if _, err := resolveExecutionCacheDir(workDir, changed); err == nil {
		t.Fatal("foreign cache key accepted")
	}
	changed.Cache.Key = ServiceCacheKey(changed.CacheScope, changed.Recipe, changed.Railpack.FrontendImage)
	if got, err := resolveExecutionCacheDir(workDir, changed); err != nil || got == dir {
		t.Fatal("different projects shared cache state")
	}
}

func TestServiceCacheBoundProtectsActiveImportsAndReplacesOldBlobs(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	a, err := openBuildCache(ctx, workDir, cacheSpec(t, "a", 100))
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.retain(ctx, cacheExport(t, 60)); err != nil {
		t.Fatal(err)
	}
	b, err := openBuildCache(ctx, workDir, cacheSpec(t, "b", 100))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	export := cacheExport(t, 60)
	if err := b.retain(ctx, export); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "index.json")); err != nil {
		t.Fatal("active cache was evicted")
	}
	if _, err := os.Stat(filepath.Join(b.dir, "index.json")); !os.IsNotExist(err) {
		t.Fatal("cache retained beyond byte budget")
	}
	a.close()
	if err := b.retain(ctx, export); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.dir); !os.IsNotExist(err) {
		t.Fatal("unused cache was not evicted")
	}
	if err := os.WriteFile(filepath.Join(b.dir, "obsolete"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.retain(ctx, cacheExport(t, 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "obsolete")); !os.IsNotExist(err) {
		t.Fatal("old local blobs accumulated after replacement")
	}
	if bytes, err := dirBytes(filepath.Join(workDir, "cache")); err != nil || bytes > 100 {
		t.Fatalf("cache bytes = %d, %v", bytes, err)
	}
}

func TestServiceCacheWaitIsCancelable(t *testing.T) {
	workDir := t.TempDir()
	spec := cacheSpec(t, "scope", 1000)
	first, err := openBuildCache(context.Background(), workDir, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := openBuildCache(ctx, workDir, spec); err == nil {
		t.Fatal("simultaneous exporter did not wait for its scope lock")
	}
}

func TestHardenedServiceCacheSurvivesWorkspaceCleanup(t *testing.T) {
	workDir := t.TempDir()
	backend := &fakeSandboxBackend{}
	executor := newHardenedExecutorForTest(workDir, backend, &fakeDaemonStarter{})
	spec := cacheSpec(t, "project/environment/service", 1000)
	for _, id := range []string{"build-1", "build-2"} {
		spec.BuildID = id
		build := emulateBuildctl(t, workDir, id, "sha256:abc")
		backend.runHook = func(ctx context.Context, net SandboxNet, step SandboxStep) error {
			for _, mount := range step.Mounts {
				if mount.Dest == sandboxGuestCache && !mount.ReadOnly {
					t.Fatal("retained cache must be a read-only import")
				}
			}
			dir := filepath.Join(workDir, id, "scratch", "cache-export")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{}"), 0o600); err != nil {
				return err
			}
			return build(ctx, net, step)
		}
		if _, err := executor.Execute(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
	}
	steps := backend.recordedSteps()
	if strings.Contains(strings.Join(steps[0].Argv, " "), "--import-cache") {
		t.Fatal("cold cache was imported")
	}
	if !strings.Contains(strings.Join(steps[1].Argv, " "), "--import-cache type=local,src=/build/cache") {
		t.Fatal("second build did not import persisted layers")
	}
}
