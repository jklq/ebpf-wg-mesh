//go:build linux

package builder

import (
	"ebof-wg-mesh/internal/config"
	"os"
	"testing"
)

// Run in the privileged Linux runtime fixture after pre-pulling busybox into
// the compact-bench namespace. The eager case is the previous startup path.
func BenchmarkHardenedStartup(b *testing.B) {
	if os.Getenv("COMPACT_BENCH_SANDBOX") != "1" {
		b.Skip("run in the Linux runtime fixture with COMPACT_BENCH_SANDBOX=1")
	}
	cfg := config.BuilderConfig{Executor: ExecutorHardened, WorkDir: b.TempDir(), Sandbox: config.BuilderSandboxConfig{Backend: "containerd", Socket: "/run/containerd/containerd.sock", Namespace: "compact-bench", Image: sandboxProbeImage, Runtime: "io.containerd.runc.v2", Snapshotter: "overlayfs", CNIPluginDir: "/usr/lib/cni", CNIConfDir: b.TempDir(), CNINetwork: "build-sandbox", BuildkitdBinary: "buildkitd"}}
	for _, mode := range []string{"eager", "on-demand"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if mode == "eager" {
					backend, err := NewSandboxBackend(sandboxConfig(cfg))
					if err != nil {
						b.Fatal(err)
					}
					_ = newHardenedExecutor(cfg.WorkDir, backend, cfg.Sandbox.BuildkitdBinary, nil)
					if err := backend.Close(); err != nil {
						b.Fatal(err)
					}
				} else {
					executor, err := newExecutorForConfig(cfg)
					if err != nil {
						b.Fatal(err)
					}
					startupBenchmarkSink = executor
				}
			}
		})
	}
}

var startupBenchmarkSink BuildExecutor
