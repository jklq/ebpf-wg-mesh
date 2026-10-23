//go:build linux

package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// VerifyHost checks, before any tenant container is created, that this host
// can actually provide the policy's boundary. A missing prerequisite is a
// startup failure with the remedy in the message: isolation never degrades
// to whatever runtime happens to be installed.
func (p Policy) VerifyHost() error {
	if p.class == "" {
		return fmt.Errorf("isolation policy is not resolved")
	}
	if _, err := exec.LookPath(p.ShimBinary()); err != nil {
		return fmt.Errorf("isolation class %q requires containerd shim %q on PATH: install it on this host (there is no fallback runtime): %w", p.class, p.ShimBinary(), err)
	}
	// The shim execs the runtime binary by the same short names at container
	// start; check it now rather than at first tenant.
	runtimeBinary := p.WorkerBinary()
	if p.class == ClassKata {
		runtimeBinary = "kata-runtime"
	}
	if _, err := exec.LookPath(runtimeBinary); err != nil {
		return fmt.Errorf("isolation class %q requires runtime binary %q on PATH: install it on this host (there is no fallback runtime): %w", p.class, runtimeBinary, err)
	}
	if p.UsesUserNamespaces() {
		if err := verifyUserNamespaces(); err != nil {
			return err
		}
	}
	if p.class == ClassKata {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			return fmt.Errorf("isolation class %q requires /dev/kvm on this host: %w", p.class, err)
		}
	}
	return nil
}

func verifyUserNamespaces() error {
	raw, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return fmt.Errorf("isolation class %q requires kernel user namespaces: %w", ClassRuncUserNS, err)
	}
	maximum, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || maximum <= 0 {
		return fmt.Errorf("isolation class %q requires kernel user namespaces: /proc/sys/user/max_user_namespaces is %q", ClassRuncUserNS, strings.TrimSpace(string(raw)))
	}
	return nil
}
