package secretkeys

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
)

// Service is the sealed-secret backend: provider, shared key registry,
// per-environment DEKs, and sealed versions behind one handle.
type Service struct {
	provider Provider
	registry *Registry
	deks     *DEKStore
	sealed   *SealedStore
}

// Open builds the sealed-secret backend from control-plane configuration.
// The file provider serves development; production must select aws-kms (see
// config validation). Open ensures the installation's active key exists so
// every replica converges on shared key state at startup.
func Open(ctx context.Context, db *sql.DB, cfg config.SecretKeysConfig) (*Service, error) {
	provider, err := OpenProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}
	svc := New(db, provider)
	if _, err := svc.registry.EnsureActiveKey(ctx); err != nil {
		_ = provider.Close()
		return nil, fmt.Errorf("ensure active envelope key: %w", err)
	}
	return svc, nil
}

// New assembles a Service over an existing provider. Tests use it to
// substitute providers; production uses Open.
func New(db *sql.DB, provider Provider) *Service {
	registry := NewRegistry(db, provider)
	deks := NewDEKStore(db, registry)
	return &Service{provider: provider, registry: registry, deks: deks, sealed: NewSealedStore(db, deks)}
}

// Close releases provider resources.
func (s *Service) Close() error {
	if s == nil || s.provider == nil {
		return nil
	}
	return s.provider.Close()
}

// ProviderName reports the configured backend name.
func (s *Service) ProviderName() string {
	if s == nil || s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

// Provider exposes the wrap/unwrap backend, e.g. so tests can build a
// second replica handle over shared provider state.
func (s *Service) Provider() Provider {
	if s == nil {
		return nil
	}
	return s.provider
}

// Registry exposes the shared key inventory for rotation and inspection.
func (s *Service) Registry() *Registry { return s.registry }

// DEKs exposes the data-encryption key inventory for rewrap and inspection.
func (s *Service) DEKs() *DEKStore { return s.deks }

// Sealed exposes sealed-secret reads and writes.
func (s *Service) Sealed() *SealedStore { return s.sealed }

// Ready reports whether the shared key state is readable. Replicas use it
// for readiness: key state must be visible to every replica before serving.
func (s *Service) Ready(ctx context.Context) bool {
	if s == nil || s.registry == nil {
		return false
	}
	_, err := s.registry.ActiveKey(ctx)
	return err == nil
}

// OpenProvider builds the configured wrap/unwrap backend without touching
// key state. The operator CLI uses it to compose read-only commands that
// must not provision keys as a side effect.
func OpenProvider(ctx context.Context, cfg config.SecretKeysConfig) (Provider, error) {
	switch strings.TrimSpace(cfg.Provider) {
	case "", ProviderFile:
		if strings.TrimSpace(cfg.File.Directory) == "" {
			return nil, fmt.Errorf("secret file provider directory is required")
		}
		return NewFileProvider(cfg.File.Directory)
	case ProviderAWSKMS:
		return NewKMSProvider(ctx, KMSConfig{
			Region:      cfg.KMS.Region,
			Endpoint:    cfg.KMS.Endpoint,
			KeyID:       cfg.KMS.KeyID,
			Timeout:     time.Duration(cfg.KMS.TimeoutSeconds) * time.Second,
			MaxAttempts: cfg.KMS.MaxAttempts,
		})
	default:
		return nil, fmt.Errorf("unknown secret keys provider %q", cfg.Provider)
	}
}
