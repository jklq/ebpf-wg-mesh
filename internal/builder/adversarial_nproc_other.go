//go:build !linux

package builder

import "errors"

// currentUIDThreadCount is unavailable off Linux; callers fall back
// to a generous process limit.
func currentUIDThreadCount() (int64, error) {
	return 0, errors.New("UID thread count requires /proc")
}
