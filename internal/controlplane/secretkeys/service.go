package secretkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ebof-wg-mesh/internal/config"
)

// Service is the sealed-secret backend: the in-process key manager, the
// shared key registry, per-environment DEKs, and sealed versions behind one
// handle.
type Service struct {
	provider *Keyring
	registry *Registry
	deks     *DEKStore
	sealed   *SealedStore
}

// Options configures Service.Open.
type Options struct {
	// AllowGenerate permits creating a missing keyring file and
	// auto-activating its first key. Development only: production must
	// provision the keyring explicitly and activate via the keys CLI, and
	// Open fails closed when the active key or any recorded version's
	// material is missing on this replica.
	AllowGenerate bool
}

// Open builds the sealed-secret backend from control-plane configuration.
// With AllowGenerate (development) it ensures the installation's active key
// exists so every replica converges on shared key state at startup.
// Without it (production) it requires an explicitly activated key and
// verifies this replica's keyring covers every recorded version.
func Open(ctx context.Context, db *sql.DB, cfg config.SecretKeysConfig, opts Options) (*Service, error) {
	provider, err := OpenProvider(cfg, opts)
	if err != nil {
		return nil, err
	}
	svc := New(db, provider)
	if opts.AllowGenerate {
		if _, err := svc.registry.EnsureActiveKey(ctx); err != nil {
			_ = provider.Close()
			return nil, fmt.Errorf("ensure active envelope key: %w", err)
		}
		return svc, nil
	}
	if _, err := svc.registry.ActiveKey(ctx); err != nil {
		_ = provider.Close()
		if errors.Is(err, ErrActiveKeyRequired) {
			return nil, fmt.Errorf("no active envelope key: provision the keyring file %s to every replica, then run `controlplane keys activate --key-id <version>`: %w",
				cfg.KeyringPath, err)
		}
		return nil, fmt.Errorf("load active envelope key: %w", err)
	}
	if err := svc.registry.VerifyLocalCoverage(ctx); err != nil {
		_ = provider.Close()
		return nil, err
	}
	return svc, nil
}

// New assembles a Service over an existing keyring.
func New(db *sql.DB, provider *Keyring) *Service {
	registry := NewRegistry(db, provider)
	deks := NewDEKStore(db, registry)
	return &Service{provider: provider, registry: registry, deks: deks, sealed: NewSealedStore(db, deks)}
}

// Close releases provider resources.
func (s *Service) Close() error {
	if s == nil || s.provider == nil {
		return nil
	}
	s.deks.mu.Lock()
	for id, dek := range s.deks.byID {
		clear(dek[:])
		delete(s.deks.byID, id)
	}
	clear(s.deks.byScope)
	s.deks.mu.Unlock()
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
func (s *Service) Provider() *Keyring {
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

// Ready reports whether the shared key state is readable and this replica
// holds every recorded version's material. Replicas use it for readiness:
// key state must be complete on every replica before serving.
func (s *Service) Ready(ctx context.Context) bool {
	if s == nil || s.registry == nil {
		return false
	}
	if _, err := s.registry.ActiveKey(ctx); err != nil {
		return false
	}
	return s.registry.VerifyLocalCoverage(ctx) == nil
}

// OpenProvider builds the configured wrap/unwrap backend without touching
// key state. The operator CLI uses it to compose commands that must not
// provision keys as a side effect.
func OpenProvider(cfg config.SecretKeysConfig, opts Options) (*Keyring, error) {
	return NewKeyring(cfg.KeyringPath, KeyringOptions{AllowGenerate: opts.AllowGenerate})
}
