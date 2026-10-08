package deploy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ebof-wg-mesh/internal/recovery"
	"golang.org/x/sys/unix"
)

type Evidence struct {
	Database       *DatabaseStatus          `json:"database,omitempty"`
	Storage        map[string]StorageStatus `json:"storage,omitempty"`
	Recovery       *RecoveryReceipt         `json:"recovery,omitempty"`
	Fleet          *recovery.FleetInput     `json:"fleet,omitempty"`
	Point          *recovery.Point          `json:"point,omitempty"`
	Object         *recovery.Object         `json:"object,omitempty"`
	Backup         string                   `json:"backup,omitempty"`
	DataLossCutoff time.Time                `json:"dataLossCutoff,omitempty"`
}
type Progress struct {
	Plan      *Plan               `json:"plan"`
	PlanID    string              `json:"planId"`
	Started   map[string]bool     `json:"started"`
	Completed map[string]Evidence `json:"completed"`
}
type State struct {
	Generation     string             `json:"generation"`
	Recovery       *RecoveryProgress  `json:"recovery,omitempty"`
	Deleted        map[string]bool    `json:"deleted,omitempty"`
	Version        int                `json:"version"`
	InstallationID string             `json:"installationId"`
	Revision       uint64             `json:"revision"`
	Bindings       map[string]Binding `json:"bindings"`
	Placements     []Placement        `json:"placements"`
	Retained       []Placement        `json:"retained,omitempty"` // unavailable instances retain their units, credentials and budgets until retirement
	Policy         *Installation      `json:"policy,omitempty"`
	Bundle         *Release           `json:"bundle,omitempty"`
	Progress       *Progress          `json:"progress,omitempty"`
	LastBackup     Evidence           `json:"lastBackup"`
	LastRestore    *Evidence          `json:"lastRestore,omitempty"`
}
type Store struct {
	path string
	lock *os.File
	aead cipher.AEAD
}

// OpenState takes an OS advisory lock for its whole lifetime. Killing the
// writer releases the lock; a stale lock file is never interpreted as ownership.
func OpenState(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("deployment state key must contain exactly 32 raw bytes")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("deployment state path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("deployment single-writer lock: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		f.Close()
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Store{path: path, lock: f, aead: aead}, nil
}
func (s *Store) Close() error { return s.lock.Close() }

var stateAAD = []byte("ebpf-wg-mesh/installation-state/v1")

func (s *Store) Read() (State, error) {
	state := State{Version: 1, Bindings: map[string]Binding{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	n := s.aead.NonceSize()
	if len(b) < n {
		return state, fmt.Errorf("truncated encrypted deployment state")
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], stateAAD)
	if err != nil {
		return state, fmt.Errorf("authenticate deployment state: %w", err)
	}
	if err := json.Unmarshal(plain, &state); err != nil {
		return state, err
	}
	if state.Version != 1 {
		return state, fmt.Errorf("unsupported deployment state version")
	}
	if state.Bindings == nil {
		state.Bindings = map[string]Binding{}
	}
	if state.Deleted == nil {
		state.Deleted = map[string]bool{}
	}
	return state, nil
}
func (s *Store) Write(state State) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return WritePrivate(s.path, s.aead.Seal(nonce, nonce, b, stateAAD))
}
func WritePrivate(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".deployment-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
