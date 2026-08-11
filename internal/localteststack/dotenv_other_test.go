//go:build !unix

package localteststack

import "fmt"

func unixMkfifo(path string) error {
	return fmt.Errorf("mkfifo unsupported on this platform")
}
