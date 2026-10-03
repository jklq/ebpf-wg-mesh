//go:build !linux

package agent

import (
	"errors"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/volumestore"
)

func newContainerdEngine(_ config.AgentConfig, _ *volumestore.Store) (serviceEngine, error) {
	return nil, errors.New("containerd runtime requires linux")
}
