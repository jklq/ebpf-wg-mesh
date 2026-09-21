package source

import (
	"fmt"
	"strings"

	"ebof-wg-mesh/internal/config"
)

func NewSourceArchiveStore(cfg config.SourceArchiveConfig) (ArchiveStore, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = config.SourceArchiveProviderFile
	}
	switch provider {
	case config.SourceArchiveProviderFile:
		return NewFileArchiveStore(cfg.Directory)
	case config.SourceArchiveProviderS3:
		return NewS3ArchiveStore(cfg.S3)
	default:
		return nil, fmt.Errorf("unknown source archive provider %q", cfg.Provider)
	}
}
