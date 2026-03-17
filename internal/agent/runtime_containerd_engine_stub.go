//go:build !linux

package agent

import (
	"errors"

	"ebof-wg-mesh/internal/config"
)

func newContainerdEngine(_ config.AgentConfig) (serviceEngine, error) {
	return nil, errors.New("containerd runtime requires linux")
}
