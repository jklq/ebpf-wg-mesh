//go:build !linux

package meshstate

import "errors"

type Ownership struct {
	InterfaceName string
	PinDir        string
}

func Acquire(string) (*Ownership, error) { return nil, errors.New("mesh ownership requires linux") }
func (o *Ownership) Close() error        { return nil }
