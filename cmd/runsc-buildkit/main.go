// runsc-buildkit adapts BuildKit's runc CLI invocations to gVisor's runsc
// CLI so a hardened builder's Dockerfile RUN steps execute under gVisor
// (isolation class "gvisor") instead of host-kernel runc. BuildKit's OCI
// worker execs its configured binary with runc's global flags (--log,
// --log-format) and runc's `run --keep`; runsc accepts neither. This binary
// translates exactly those arguments, injects the runsc root/network/Cgroup
// policy, and execs runsc so exit codes and stdio pass through untouched.
// Any argument it does not recognize is forwarded unchanged except the
// explicitly dropped runc-only flags; an unrecognized invocation shape
// fails in runsc, never silently in a weaker runtime.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const runscBinary = "runsc"

// runscRootEnv is set by the builder to a per-execution directory.
const runscRootEnv = "PLATFORM_RUNSC_ROOT"

func main() {
	runsc, err := exec.LookPath(runscBinary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runsc-buildkit: %s not found on PATH: %v\n", runscBinary, err)
		os.Exit(127)
	}
	root := os.Getenv(runscRootEnv)
	if root == "" {
		root = filepath.Join(os.TempDir(), "runsc-buildkit")
	}
	args := append([]string{runscBinary, "--root", root, "--ignore-cgroups", "--network", "host"}, translate(os.Args[1:])...)
	if err := syscall.Exec(runsc, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "runsc-buildkit: exec runsc: %v\n", err)
		os.Exit(127)
	}
}

// translate drops runc-only arguments BuildKit emits and keeps everything
// else in order. Dropped: global --log/--log-format (runsc reports on
// stderr instead), a runc --root (replaced by ours), --systemd-cgroup, and
// `run --keep` (runsc retains state under its root until delete already).
// Both "--flag value" and "--flag=value" spellings are handled.
func translate(in []string) []string {
	out := make([]string, 0, len(in))
	for i := 0; i < len(in); i++ {
		arg := in[i]
		name, _, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--log", "--log-format", "--root":
			if !hasValue && i+1 < len(in) {
				i++
			}
		case "--systemd-cgroup", "--keep":
			// Boolean runc flags with no runsc equivalent.
		default:
			out = append(out, arg)
		}
	}
	return out
}
