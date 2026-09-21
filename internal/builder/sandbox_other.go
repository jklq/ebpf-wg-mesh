//go:build !linux

package builder

import "errors"

// errSandboxUnsupported fails closed on platforms without the Linux
// sandbox backend: untrusted builds never run without isolation.
var errSandboxUnsupported = errors.New("hardened executor requires linux: refusing to run untrusted builds without sandbox isolation")

func newSandboxBackendPlatform(SandboxBackendConfig) (SandboxBackend, error) {
	return nil, errSandboxUnsupported
}
