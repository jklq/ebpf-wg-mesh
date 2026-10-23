//go:build !linux

package isolation

import "fmt"

func (p Policy) VerifyHost() error {
	return fmt.Errorf("isolation class %q requires a Linux host", p.class)
}
