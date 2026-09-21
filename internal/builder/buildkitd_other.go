//go:build !linux

package builder

import "context"

// buildkitdProc is a stub on platforms without the Linux sandbox
// backend. startBuildkitd always fails, so these methods never run;
// they exist so the portable executor compiles everywhere.
type buildkitdProc struct{}

func startBuildkitd(context.Context, string, string, string, string, []string) (*buildkitdProc, error) {
	return nil, errSandboxUnsupported
}

func (p *buildkitdProc) waitReady(context.Context) error {
	return errSandboxUnsupported
}

func (p *buildkitdProc) Stop() error {
	return nil
}
