package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type SourceArchiveStore interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	ReadRange(context.Context, string, int64, int) ([]byte, error)
	Delete(context.Context, string) error
}

type FileSourceArchiveStore struct {
	root string
}

var sourceArchiveDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func NewFileSourceArchiveStore(root string) (*FileSourceArchiveStore, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return nil, errors.New("source archive directory is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create source archive directory: %w", err)
	}
	return &FileSourceArchiveStore{root: root}, nil
}

func sourceArchiveObjectKey(digest string) (string, error) {
	digest = strings.TrimSpace(strings.ToLower(digest))
	if !sourceArchiveDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid source archive digest %q", digest)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	return filepath.ToSlash(filepath.Join("sha256", hexDigest[:2], hexDigest+".tgz")), nil
}

func (s *FileSourceArchiveStore) path(key string) (string, error) {
	if s == nil {
		return "", errors.New("source archive store is not configured")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	return filepath.Join(s.root, clean), nil
}

func (s *FileSourceArchiveStore) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".source-archive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (s *FileSourceArchiveStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *FileSourceArchiveStore) ReadRange(ctx context.Context, key string, offset int64, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || limit <= 0 {
		return nil, errors.New("invalid source archive range")
	}
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if offset >= info.Size() {
		return []byte{}, nil
	}
	if remaining := info.Size() - offset; remaining < int64(limit) {
		limit = int(remaining)
	}
	chunk := make([]byte, limit)
	read, err := file.ReadAt(chunk, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return chunk[:read], nil
}

func (s *FileSourceArchiveStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
