package secretkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ebof-wg-mesh/internal/config"
)

// Service is the envelope-encryption backend: the key manager, key registry,
// and per-environment DEKs behind one handle.
type Service struct {
	provider *Keyring
	registry *Registry
	deks     *DEKStore
}

// Options configures Service.Open.
type Options struct {
	// AllowGenerate permits creating a missing keyring file and auto-activating its
	// first key. Development only: production provisions explicitly and activates via
	// the keys CLI, and Open fails closed when any material is missing.
	AllowGenerate bool
}

// Open builds the envelope-encryption backend from control-plane configuration. With
// AllowGenerate (development) it ensures the active key exists so replicas converge
// at startup. Without it (production) it requires an explicitly activated key and
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

func New(db *sql.DB, provider *Keyring) *Service {
	registry := NewRegistry(db, provider)
	deks := NewDEKStore(db, registry)
	return &Service{provider: provider, registry: registry, deks: deks}
}

// Close clears cached DEKs and releases provider resources.
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

func (s *Service) ProviderName() string {
	if s == nil || s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

// Provider exposes the wrap/unwrap backend, e.g. so tests can build a second replica handle.
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

// Ciphertext is a value encrypted under one scope's data-encryption key.
type Ciphertext struct {
	DEKID string
	// Data is the nonce followed by the AES-256-GCM ciphertext.
	Data []byte
}

// Encrypt encrypts plaintext under the scope's DEK, minting the DEK inside
// q's transaction on first use. aad binds the ciphertext to its storage location.
func (s *Service) Encrypt(ctx context.Context, q Querier, scope Scope, aad, plaintext []byte) (Ciphertext, error) {
	dek, dekID, err := s.deks.DEKForScope(ctx, q, scope.Kind, scope.ID)
	if err != nil {
		return Ciphertext{}, err
	}
	defer clear(dek[:])
	data, err := EncryptValue(dek, aad, plaintext)
	if err != nil {
		return Ciphertext{}, err
	}
	return Ciphertext{DEKID: dekID, Data: data}, nil
}

// Decrypt opens a Ciphertext under the aad it was encrypted with. The plaintext
// is runtime-only; callers must not log or persist it.
func (s *Service) Decrypt(ctx context.Context, q Querier, c Ciphertext, aad []byte) ([]byte, error) {
	dek, err := s.deks.DEKByID(ctx, q, c.DEKID)
	if err != nil {
		return nil, err
	}
	defer clear(dek[:])
	return DecryptValue(dek, aad, c.Data)
}

// Ready reports whether shared key state is readable and this replica holds every
// recorded version's material. Replicas use it for readiness: key state must be complete before serving.
func (s *Service) Ready(ctx context.Context) bool {
	if s == nil || s.registry == nil {
		return false
	}
	if _, err := s.registry.ActiveKey(ctx); err != nil {
		return false
	}
	return s.registry.VerifyLocalCoverage(ctx) == nil
}

// OpenProvider builds the configured wrap/unwrap backend without touching key state.
// The operator CLI uses it for commands that must not provision keys as a side effect.
func OpenProvider(cfg config.SecretKeysConfig, opts Options) (*Keyring, error) {
	return NewKeyring(cfg.KeyringPath, KeyringOptions{AllowGenerate: opts.AllowGenerate})
}
