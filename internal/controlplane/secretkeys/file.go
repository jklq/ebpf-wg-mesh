package secretkeys

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// fileProviderVersion is the on-disk version of the development key file.
const fileProviderVersion = 1

// fileWrapVersion prefixes wrapped bytes produced by the file provider.
const fileWrapVersion byte = 0x01

const fileKeysFileName = "keys.json"

// FileProvider is the development-only KeyProvider. Root keys are AES-256
// keys held in a JSON file under a state directory. Production must reject
// this provider: the file cannot be shared consistently across replicas and
// offers no audit or access boundary. See config validation.
type FileProvider struct {
	path string
}

// NewFileProvider opens (creating if needed) the development key file under
// dir. The file is created with 0600 permissions; an existing file with
// group/other access is left alone but never loosened.
func NewFileProvider(dir string) (*FileProvider, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("secret file provider directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secret key directory: %w", err)
	}
	path := filepath.Join(dir, fileKeysFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		empty, err := json.Marshal(fileKeyStore{Version: fileProviderVersion, Keys: map[string]fileKey{}})
		if err != nil {
			return nil, fmt.Errorf("encode empty secret key file: %w", err)
		}
		if err := os.WriteFile(path, append(empty, '\n'), 0o600); err != nil {
			return nil, fmt.Errorf("create secret key file: %w", err)
		}
	}
	return &FileProvider{path: path}, nil
}

// Name implements Provider.
func (p *FileProvider) Name() string { return ProviderFile }

// ProvisionKey implements Provider. The hint is ignored: the file provider
// generates its own material and returns a fresh reference. Concurrent
// replicas racing to provision converge on distinct references; the registry
// decides which one becomes active, and unreferenced file keys are inert.
func (p *FileProvider) ProvisionKey(ctx context.Context, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate file key reference: %w", err)
	}
	ref := "file-" + hex.EncodeToString(raw[:])
	key := make([]byte, DEKSize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate file key material: %w", err)
	}
	if err := p.withStore(true, func(store *fileKeyStore) error {
		store.Keys[ref] = fileKey{
			Algorithm: "AES-256-GCM",
			CreatedAt: time.Now().UTC(),
			Key:       base64.StdEncoding.EncodeToString(key),
		}
		return nil
	}); err != nil {
		return "", err
	}
	return ref, nil
}

// Wrap implements Provider.
func (p *FileProvider) Wrap(ctx context.Context, ref string, plaintext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("file provider requires a key reference")
	}
	key, err := p.keyForRef(ref)
	if err != nil {
		return nil, err
	}
	// Bind the wrap to the key reference so wrapped bytes cannot be
	// transplanted across keys.
	sealed, err := sealWithNonce(key, []byte("file-wrap/v1\x00"+ref), plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("file wrap: %w", err)
	}
	out := make([]byte, 0, 1+len(sealed))
	out = append(out, fileWrapVersion)
	return append(out, sealed...), nil
}

// Unwrap implements Provider.
func (p *FileProvider) Unwrap(ctx context.Context, ref string, wrapped []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("file provider requires a key reference")
	}
	if len(wrapped) < 1 || wrapped[0] != fileWrapVersion {
		return nil, fmt.Errorf("%w: file wrapped key has unknown version", ErrCiphertextInvalid)
	}
	key, err := p.keyForRef(ref)
	if err != nil {
		return nil, err
	}
	payload := wrapped[1:]
	if len(payload) < NonceSize {
		return nil, fmt.Errorf("%w: file wrapped key is truncated", ErrCiphertextInvalid)
	}
	aead, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, payload[:NonceSize], payload[NonceSize:], []byte("file-wrap/v1\x00"+ref))
	if err != nil {
		return nil, fmt.Errorf("%w: file wrapped key", ErrCiphertextInvalid)
	}
	return plaintext, nil
}

// Close implements Provider.
func (p *FileProvider) Close() error { return nil }

// Path reports the key file location for diagnostics.
func (p *FileProvider) Path() string { return p.path }

func (p *FileProvider) keyForRef(ref string) ([]byte, error) {
	var encoded string
	if err := p.withStore(false, func(store *fileKeyStore) error {
		entry, ok := store.Keys[ref]
		if !ok {
			return fmt.Errorf("%w: file key %q", ErrProviderKeyNotFound, ref)
		}
		encoded = entry.Key
		return nil
	}); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("file key %q is corrupt: %w", ref, err)
	}
	if len(key) != DEKSize {
		return nil, fmt.Errorf("file key %q has invalid length", ref)
	}
	return key, nil
}

type fileKey struct {
	Algorithm string    `json:"algorithm"`
	CreatedAt time.Time `json:"created_at"`
	Key       string    `json:"key"`
}

type fileKeyStore struct {
	Version int                `json:"version"`
	Keys    map[string]fileKey `json:"keys"`
}

// withStore runs fn under a flock-guarded read-modify-write of the key
// file. Replicas share the state directory today, so the lock (not the
// process) serializes writers. The lock lives on a sidecar file that is
// never renamed: locking the data file itself would race atomic renames,
// since a rename swaps the inode out from under a held lock.
func (p *FileProvider) withStore(write bool, fn func(*fileKeyStore) error) error {
	lockFile, err := os.OpenFile(p.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open secret key lock file: %w", err)
	}
	defer lockFile.Close()
	lock := unix.LOCK_SH
	if write {
		lock = unix.LOCK_EX
	}
	if err := unix.Flock(int(lockFile.Fd()), lock); err != nil {
		return fmt.Errorf("lock secret key file: %w", err)
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("read secret key file: %w", err)
	}
	var store fileKeyStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return fmt.Errorf("decode secret key file: %w", err)
	}
	if store.Version != fileProviderVersion {
		return fmt.Errorf("secret key file has unsupported version %d", store.Version)
	}
	if store.Keys == nil {
		store.Keys = map[string]fileKey{}
	}
	if err := fn(&store); err != nil {
		return err
	}
	if !write {
		return nil
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("encode secret key file: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), "keys-*.json.tmp")
	if err != nil {
		return fmt.Errorf("stage secret key file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write secret key file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("protect secret key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close secret key file: %w", err)
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace secret key file: %w", err)
	}
	return nil
}
