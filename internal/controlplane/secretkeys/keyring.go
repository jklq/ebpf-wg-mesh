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
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// keyringFileVersion is the on-disk version of the provisioned keyring file.
const keyringFileVersion = 1

// keyringWrapVersion prefixes wrapped bytes produced by the keyring provider.
const keyringWrapVersion byte = 0x01

// keyringWrapContext prefixes the additional authenticated data binding a
// wrap to its key reference and purpose.
const keyringWrapContext = "keyring-wrap/v1"

// MaxKeyVersionLength caps keyring key version IDs.
const MaxKeyVersionLength = 128

// keyVersionPattern restricts version IDs to operator-friendly text without
// whitespace or path separators.
var keyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// ValidateKeyVersionID rejects empty, overlong, or oddly shaped key version
// IDs before they reach the keyring file or the registry.
func ValidateKeyVersionID(id string) error {
	if len(id) == 0 || len(id) > MaxKeyVersionLength || !keyVersionPattern.MatchString(id) {
		return fmt.Errorf("key version %q must match [A-Za-z0-9_][A-Za-z0-9_.-]* and be 1-%d characters",
			id, MaxKeyVersionLength)
	}
	return nil
}

// KeyringOptions configures the in-process key manager.
type KeyringOptions struct {
	// AllowGenerate permits creating a missing keyring file and minting
	// its first key during development bootstrap. Production must leave
	// this false: missing production keys are an operator error, never an
	// auto-generated fallback.
	AllowGenerate bool
}

// Keyring is the in-process Provider. Versioned AES-256 master keys live in
// an explicitly provisioned JSON file that the operator replicates to every
// control-plane replica, separately from the database. Wrap and unwrap run
// in process; no network call and no manual unlock happen on restart.
//
// The provider reads the file through on every operation, so provisioning a
// new version to a running replica's file takes effect without a restart.
// Generation happens only through GenerateKey (the explicit `keys
// provision` operator command) and, when AllowGenerate is set, the
// development bootstrap path.
type Keyring struct {
	path          string
	allowGenerate bool
}

// NewKeyring opens the provisioned keyring file at path. It validates the
// path only; the file itself is read (and validated) on every operation so
// running replicas observe newly provisioned versions.
func NewKeyring(path string, opts KeyringOptions) (*Keyring, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("secret keyring path is required")
	}
	return &Keyring{path: path, allowGenerate: opts.AllowGenerate}, nil
}

// Name implements Provider.
func (k *Keyring) Name() string { return ProviderKeyring }

// ProvisionKey implements Provider. hint must name a key version already
// provisioned to this replica's keyring file; ProvisionKey verifies the
// material round-trips and returns the hint as the reference. It never
// generates material: provision the version to every replica before
// activating it.
func (k *Keyring) ProvisionKey(ctx context.Context, hint string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref := strings.TrimSpace(hint)
	if ref == "" {
		return "", errors.New("keyring provision requires a key version: provision it with `controlplane keys provision`, copy the keyring file to every replica, then activate it")
	}
	if err := ValidateKeyVersionID(ref); err != nil {
		return "", err
	}
	proof := make([]byte, DEKSize)
	if _, err := rand.Read(proof); err != nil {
		return "", fmt.Errorf("generate keyring proof: %w", err)
	}
	wrapped, err := k.Wrap(ctx, ref, keyringProofPurpose(ref), proof)
	if err != nil {
		return "", err
	}
	opened, err := k.Unwrap(ctx, ref, keyringProofPurpose(ref), wrapped)
	if err != nil {
		return "", fmt.Errorf("verify keyring version %q: %w", ref, err)
	}
	if len(opened) != len(proof) {
		return "", fmt.Errorf("verify keyring version %q: round-trip mismatch", ref)
	}
	return ref, nil
}

// Wrap implements Provider. The ciphertext is bound to both the key
// reference and the purpose, so wrapped bytes cannot be transplanted across
// keys or records.
func (k *Keyring) Wrap(ctx context.Context, ref, purpose string, plaintext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("keyring wrap requires a key reference")
	}
	if strings.TrimSpace(purpose) == "" {
		return nil, errors.New("keyring wrap requires a purpose context")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("keyring wrap requires key material")
	}
	key, err := k.keyForRef(ref)
	if err != nil {
		return nil, err
	}
	sealed, err := sealWithNonce(key, keyringAAD(ref, purpose), plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("keyring wrap: %w", err)
	}
	out := make([]byte, 0, 1+len(sealed))
	out = append(out, keyringWrapVersion)
	return append(out, sealed...), nil
}

// Unwrap implements Provider.
func (k *Keyring) Unwrap(ctx context.Context, ref, purpose string, wrapped []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("keyring unwrap requires a key reference")
	}
	if strings.TrimSpace(purpose) == "" {
		return nil, errors.New("keyring unwrap requires a purpose context")
	}
	if len(wrapped) < 1 || wrapped[0] != keyringWrapVersion {
		return nil, fmt.Errorf("%w: keyring wrapped key has unknown version", ErrCiphertextInvalid)
	}
	key, err := k.keyForRef(ref)
	if err != nil {
		return nil, err
	}
	payload := wrapped[1:]
	if len(payload) < NonceSize {
		return nil, fmt.Errorf("%w: keyring wrapped key is truncated", ErrCiphertextInvalid)
	}
	aead, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, payload[:NonceSize], payload[NonceSize:], keyringAAD(ref, purpose))
	if err != nil {
		return nil, fmt.Errorf("%w: keyring wrapped key", ErrCiphertextInvalid)
	}
	return plaintext, nil
}

// Close implements Provider.
func (k *Keyring) Close() error { return nil }

// Path reports the keyring file location for diagnostics. It never appears
// alongside key material.
func (k *Keyring) Path() string { return k.path }

// HasKeyMaterial implements MaterialChecker. It reports whether this
// replica's keyring file holds validated material for ref. A missing or
// unreadable file is an error, not an absence: callers must distinguish "no
// such version" from "cannot read the keyring".
func (k *Keyring) HasKeyMaterial(_ context.Context, ref string) (bool, error) {
	var present bool
	if err := k.withStore(false, func(store *keyringStore) error {
		entry, ok := store.Keys[ref]
		if !ok {
			return nil
		}
		if err := validateKeyringEntry(ref, entry); err != nil {
			return err
		}
		present = true
		return nil
	}); err != nil {
		return false, err
	}
	return present, nil
}

// LocalKeyVersions returns the validated key version IDs this replica's
// keyring file holds, for operator inspection and coverage checks.
func (k *Keyring) LocalKeyVersions() ([]string, error) {
	var out []string
	if err := k.withStore(false, func(store *keyringStore) error {
		for id, entry := range store.Keys {
			if err := ValidateKeyVersionID(id); err != nil {
				return fmt.Errorf("keyring key %q: %w", id, err)
			}
			if err := validateKeyringEntry(id, entry); err != nil {
				return err
			}
			out = append(out, id)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureBootstrapKey implements BootstrapProvisioner. With AllowGenerate it
// creates a missing keyring file, mints the first version when the file
// holds none, and returns the single version when the file holds exactly
// one. Without AllowGenerate it fails closed: production keys are
// provisioned explicitly and activated with `controlplane keys activate`.
func (k *Keyring) EnsureBootstrapKey(_ context.Context) (string, error) {
	if !k.allowGenerate {
		return "", fmt.Errorf("%w: provision the keyring file %s to this replica, then run `controlplane keys activate --key-id <version>`",
			ErrActiveKeyRequired, k.path)
	}
	var ref string
	if err := k.withStore(true, func(store *keyringStore) error {
		switch len(store.Keys) {
		case 0:
			id, entry, err := generateKeyringEntry("")
			if err != nil {
				return err
			}
			store.Keys[id] = entry
			ref = id
		case 1:
			for id, entry := range store.Keys {
				if err := validateKeyringEntry(id, entry); err != nil {
					return err
				}
				ref = id
			}
		default:
			return fmt.Errorf("%w: keyring %s holds %d versions; activate one explicitly with `controlplane keys activate --key-id <version>`",
				ErrActiveKeyRequired, k.path, len(store.Keys))
		}
		return nil
	}); err != nil {
		return "", err
	}
	return ref, nil
}

// GenerateKey mints a fresh AES-256 master key version and appends it to
// this replica's keyring file. An empty id generates a random version ID;
// a provided id must be unique in the file. This is the explicit operator
// `keys provision` step: it touches only the local file, so the operator
// copies the updated file to every replica before activating the version.
// It never writes key material anywhere but the 0600 keyring file.
func (k *Keyring) GenerateKey(_ context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		generated, err := generateKeyVersionID()
		if err != nil {
			return "", err
		}
		id = generated
	}
	if err := ValidateKeyVersionID(id); err != nil {
		return "", err
	}
	if err := k.withStore(true, func(store *keyringStore) error {
		if _, exists := store.Keys[id]; exists {
			return fmt.Errorf("keyring version %q already exists in %s", id, k.path)
		}
		_, entry, err := generateKeyringEntry(id)
		if err != nil {
			return err
		}
		store.Keys[id] = entry
		return nil
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (k *Keyring) keyForRef(ref string) ([]byte, error) {
	var encoded string
	if err := k.withStore(false, func(store *keyringStore) error {
		entry, ok := store.Keys[ref]
		if !ok {
			return fmt.Errorf("%w: keyring version %q is not provisioned on this replica",
				ErrProviderKeyNotFound, ref)
		}
		if err := validateKeyringEntry(ref, entry); err != nil {
			return err
		}
		encoded = entry.Key
		return nil
	}); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Unreachable: validateKeyringEntry already decoded it. Kept to
		// fail closed rather than trust the earlier read.
		return nil, fmt.Errorf("keyring version %q is corrupt", ref)
	}
	return key, nil
}

type keyringEntry struct {
	Algorithm string    `json:"algorithm"`
	CreatedAt time.Time `json:"created_at"`
	Key       string    `json:"key"`
}

type keyringStore struct {
	Version int                     `json:"version"`
	Keys    map[string]keyringEntry `json:"keys"`
}

// validateKeyringEntry checks one entry's shape and decodes its material
// without returning it. Errors name the version, never the material.
func validateKeyringEntry(id string, entry keyringEntry) error {
	if err := ValidateKeyVersionID(id); err != nil {
		return err
	}
	if entry.Algorithm != "AES-256-GCM" {
		return fmt.Errorf("keyring version %q has unsupported algorithm %q", id, entry.Algorithm)
	}
	key, err := base64.StdEncoding.DecodeString(entry.Key)
	if err != nil {
		return fmt.Errorf("keyring version %q is not valid base64", id)
	}
	if len(key) != DEKSize {
		return fmt.Errorf("keyring version %q must hold a 256-bit key", id)
	}
	return nil
}

func generateKeyVersionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate key version id: %w", err)
	}
	return "kr-" + hex.EncodeToString(raw[:]), nil
}

func generateKeyringEntry(id string) (string, keyringEntry, error) {
	if id == "" {
		generated, err := generateKeyVersionID()
		if err != nil {
			return "", keyringEntry{}, err
		}
		id = generated
	}
	key := make([]byte, DEKSize)
	if _, err := rand.Read(key); err != nil {
		return "", keyringEntry{}, fmt.Errorf("generate keyring material: %w", err)
	}
	return id, keyringEntry{
		Algorithm: "AES-256-GCM",
		CreatedAt: time.Now().UTC(),
		Key:       base64.StdEncoding.EncodeToString(key),
	}, nil
}

func keyringAAD(ref, purpose string) []byte {
	return []byte(keyringWrapContext + "\x00" + ref + "\x00" + purpose)
}

func keyringProofPurpose(ref string) string {
	return "keyring-proof/v1/" + ref
}

// withStore runs fn under a flock-guarded read of the keyring file,
// optionally followed by an atomic rewrite. Readers take a shared lock;
// writers take an exclusive lock and replace the file with a 0600
// temp-and-rename. The lock lives on a sidecar file that is never renamed:
// locking the data file itself would race atomic renames, since a rename
// swaps the inode out from under a held lock.
//
// Every load validates file access (regular file, no group/other
// permission bits) and the store version, and every entry is validated on
// access, so a misprovisioned replica fails closed instead of wrapping
// under weak or ambiguous material.
func (k *Keyring) withStore(write bool, fn func(*keyringStore) error) error {
	if write {
		if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
			return fmt.Errorf("create keyring directory: %w", err)
		}
	}
	lockFile, err := os.OpenFile(k.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open keyring lock file: %w", err)
	}
	defer lockFile.Close()
	lock := unix.LOCK_SH
	if write {
		lock = unix.LOCK_EX
	}
	if err := unix.Flock(int(lockFile.Fd()), lock); err != nil {
		return fmt.Errorf("lock keyring file: %w", err)
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	raw, err := os.ReadFile(k.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && write {
			store := &keyringStore{Version: keyringFileVersion, Keys: map[string]keyringEntry{}}
			if err := fn(store); err != nil {
				return err
			}
			return k.rewriteStore(store)
		}
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: keyring file %s is not provisioned on this replica",
				ErrProviderKeyNotFound, k.path)
		}
		return fmt.Errorf("read keyring file: %w", err)
	}
	info, err := os.Stat(k.path)
	if err != nil {
		return fmt.Errorf("stat keyring file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("keyring file %s is not a regular file", k.path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("keyring file %s must not grant group/other access (mode %04o); run chmod 0600 %s",
			k.path, perm, k.path)
	}
	var store keyringStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return fmt.Errorf("decode keyring file %s: %w", k.path, err)
	}
	if store.Version != keyringFileVersion {
		return fmt.Errorf("keyring file %s has unsupported version %d", k.path, store.Version)
	}
	if store.Keys == nil {
		store.Keys = map[string]keyringEntry{}
	}
	if err := fn(&store); err != nil {
		return err
	}
	if !write {
		return nil
	}
	return k.rewriteStore(&store)
}

func (k *Keyring) rewriteStore(store *keyringStore) error {
	encoded, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("encode keyring file: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(k.path), "keys-*.json.tmp")
	if err != nil {
		return fmt.Errorf("stage keyring file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write keyring file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("protect keyring file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close keyring file: %w", err)
	}
	if err := os.Rename(tmpName, k.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace keyring file: %w", err)
	}
	return nil
}
