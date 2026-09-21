package source

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func ArchiveDigest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

type ObjectMetadata struct {
	Size   int64
	Digest string
}

type ArchiveStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, digest string) error
	Open(ctx context.Context, key string) (io.ReadCloser, ObjectMetadata, error)
	ReadRange(ctx context.Context, key string, offset int64, limit int) ([]byte, error)
	Stat(ctx context.Context, key string) (ObjectMetadata, error)
	Delete(ctx context.Context, key string) error
}

type FileArchiveStore struct {
	root string
}

var sourceArchiveDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func NewFileArchiveStore(root string) (*FileArchiveStore, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return nil, errors.New("source archive directory is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create source archive directory: %w", err)
	}
	return &FileArchiveStore{root: root}, nil
}

func (s *FileArchiveStore) Ready() bool {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return false
	}
	info, err := os.Stat(s.root)
	return err == nil && info.IsDir()
}

func ArchiveObjectKey(digest string) (string, error) {
	digest = strings.TrimSpace(strings.ToLower(digest))
	if !sourceArchiveDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid source archive digest %q", digest)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	return filepath.ToSlash(filepath.Join("sha256", hexDigest[:2], hexDigest+".tgz")), nil
}

func DigestFromObjectKey(key string) (string, error) {
	clean := strings.Trim(strings.TrimSpace(key), "/")
	parts := strings.Split(clean, "/")
	if len(parts) != 3 || parts[0] != "sha256" {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	hexDigest := strings.TrimSuffix(parts[2], ".tgz")
	digest := "sha256:" + strings.ToLower(hexDigest)
	if !sourceArchiveDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	if parts[1] != hexDigest[:2] {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	return digest, nil
}

func (s *FileArchiveStore) path(key string) (string, error) {
	if s == nil {
		return "", errors.New("source archive store is not configured")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	return filepath.Join(s.root, clean), nil
}

func (s *FileArchiveStore) Put(ctx context.Context, key string, body io.Reader, size int64, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if body == nil {
		return errors.New("source archive body is required")
	}
	digest = strings.TrimSpace(strings.ToLower(digest))
	if !sourceArchiveDigestPattern.MatchString(digest) {
		return fmt.Errorf("%w: digest %q is invalid", ErrArchiveCorrupt, digest)
	}
	wantKey, err := ArchiveObjectKey(digest)
	if err != nil {
		return err
	}
	if strings.TrimSpace(key) != wantKey {
		return fmt.Errorf("%w: object key does not match digest", ErrArchiveCorrupt)
	}
	if size <= 0 || size > MaxArchiveCompressedBytes {
		return fmt.Errorf("%w: size %d bytes", ErrArchiveTooLarge, size)
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		if info.Size() != size {
			return fmt.Errorf("%w: existing object size %d differs from %d", ErrArchiveCorrupt, info.Size(), size)
		}
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
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(tmp, hash), body, size+1)
	if err != nil && !errors.Is(err, io.EOF) {
		tmp.Close()
		return fmt.Errorf("write source archive object: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		tmp.Close()
		return ctxErr
	}
	if written != size {
		tmp.Close()
		return fmt.Errorf("%w: declared size %d but body yielded %d bytes", ErrArchiveCorrupt, size, written)
	}
	if extra, err := io.Copy(io.Discard, io.LimitReader(body, 1)); err != nil {
		tmp.Close()
		return fmt.Errorf("verify source archive body length: %w", err)
	} else if extra != 0 {
		tmp.Close()
		return fmt.Errorf("%w: declared size %d but body is longer", ErrArchiveCorrupt, size)
	}
	if actual := fmt.Sprintf("sha256:%x", hash.Sum(nil)); actual != digest {
		tmp.Close()
		return fmt.Errorf("%w: digest verification failed", ErrArchiveCorrupt)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			if info.Size() != size {
				return fmt.Errorf("%w: existing object size %d differs from %d", ErrArchiveCorrupt, info.Size(), size)
			}
			return nil
		}
		return err
	}
	return nil
}

func (s *FileArchiveStore) Open(ctx context.Context, key string) (io.ReadCloser, ObjectMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectMetadata{}, err
	}
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	path, err := s.path(key)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ObjectMetadata{}, fmt.Errorf("%w: %s: %w", ErrArchiveNotFound, key, os.ErrNotExist)
		}
		return nil, ObjectMetadata{}, err
	}
	return file, meta, nil
}

func (s *FileArchiveStore) Stat(ctx context.Context, key string) (ObjectMetadata, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMetadata{}, err
	}
	digest, err := DigestFromObjectKey(key)
	if err != nil {
		return ObjectMetadata{}, err
	}
	path, err := s.path(key)
	if err != nil {
		return ObjectMetadata{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectMetadata{}, fmt.Errorf("%w: %s: %w", ErrArchiveNotFound, key, os.ErrNotExist)
		}
		return ObjectMetadata{}, err
	}
	return ObjectMetadata{Size: info.Size(), Digest: digest}, nil
}

func (s *FileArchiveStore) ReadRange(ctx context.Context, key string, offset int64, limit int) ([]byte, error) {
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
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s: %w", ErrArchiveNotFound, key, os.ErrNotExist)
		}
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
	if int64(read) < int64(limit) {
		if current, statErr := os.Stat(path); statErr == nil && current.Size() != info.Size() {
			return nil, fmt.Errorf("%w: source snapshot archive changed while streaming", ErrArchiveCorrupt)
		}
	}
	return chunk[:read], nil
}

func (s *FileArchiveStore) Delete(ctx context.Context, key string) error {
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
