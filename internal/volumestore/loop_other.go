//go:build !linux

package volumestore

import "errors"

var errLoopUnsupported = errors.New("loop volumes require Linux; use the directory volume backend")

type loopBackend struct{}

func (loopBackend) ensure(string, int64) error      { return errLoopUnsupported }
func (loopBackend) dataPath(string) (string, error) { return "", errLoopUnsupported }
func (loopBackend) destroy(string) error            { return errLoopUnsupported }
func (loopBackend) usage(string, int64) (int64, int64, int64, error) {
	return 0, 0, 0, errLoopUnsupported
}
