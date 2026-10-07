//go:build !linux

package recovery

import "fmt"

func prepareIsolation(string) error { return fmt.Errorf("isolated recovery requires Linux namespaces") }
