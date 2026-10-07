//go:build !linux

package recovery

import "fmt"

func mountOfflineWorkspace(string) error {
	return fmt.Errorf("native offline recovery requires Linux namespaces")
}
