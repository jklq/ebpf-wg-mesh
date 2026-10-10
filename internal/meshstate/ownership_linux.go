//go:build linux

// Package meshstate coordinates ownership of the host's persistent data plane.
package meshstate

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const pinRoot = "/sys/fs/bpf/ebpf-wg-mesh"

func PinPath(interfaceName string) (string, error) {
	if interfaceName == "" || interfaceName == "." || interfaceName == ".." || filepath.Base(interfaceName) != interfaceName {
		return "", fmt.Errorf("invalid mesh interface name %q", interfaceName)
	}
	return filepath.Join(pinRoot, interfaceName), nil
}

type Ownership struct {
	InterfaceName string
	PinDir        string
	lock          *flock.Flock
}

// Acquire fences startup, reconciliation and explicit removal before any
// WireGuard or firewall state can be changed by a second process.
func Acquire(interfaceName string) (*Ownership, error) {
	pinDir, err := PinPath(interfaceName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll("/run/ebpf-wg-mesh", 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join("/run/ebpf-wg-mesh", interfaceName+".lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("mesh data plane %s is already managed by another process", interfaceName)
	}
	return &Ownership{InterfaceName: interfaceName, PinDir: pinDir, lock: lock}, nil
}

func (o *Ownership) Close() error {
	if o == nil {
		return nil
	}
	return o.lock.Unlock()
}
