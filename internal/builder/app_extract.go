package builder

import (
	"archive/tar"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func cooperativeBuildCancel(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.FailedPrecondition, codes.NotFound:
		return true
	default:
		return false
	}
}

func extractSourceSnapshotReader(repoDir string, archive io.Reader, compressedSize int64) error {
	if compressedSize <= 0 {
		return errors.New("snapshot archive is empty")
	}
	if compressedSize > maxSourceArchiveCompressedBytes {
		return errors.New("snapshot archive exceeds compressed size limit")
	}
	gzr, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open snapshot archive: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	var totalBytes int64
	var entries int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read snapshot archive: %w", err)
		}
		entries++
		if entries > maxSourceArchiveEntries {
			return errors.New("snapshot archive contains too many entries")
		}
		if hdr.Size < 0 || hdr.Size > maxSourceArchiveFileBytes {
			return fmt.Errorf("snapshot entry %q exceeds file size limit", hdr.Name)
		}
		if hdr.Size > maxSourceArchiveExpandedBytes-totalBytes {
			return errors.New("snapshot archive exceeds expanded size limit")
		}
		totalBytes += hdr.Size
		name := strings.TrimSpace(hdr.Name)
		if name == "" {
			continue
		}
		rel, ok := stripArchiveRoot(name)
		if !ok || rel == "" {
			continue
		}
		target, err := safeArchivePath(repoDir, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir snapshot dir: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir snapshot parent: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return fmt.Errorf("create snapshot file: %w", err)
			}
			if _, err := io.CopyN(file, tr, hdr.Size); err != nil {
				_ = file.Close()
				return fmt.Errorf("write snapshot file: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close snapshot file: %w", err)
			}
		default:
			return fmt.Errorf("unsupported snapshot entry type %d", hdr.Typeflag)
		}
	}
}

func stripArchiveRoot(name string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == "/" {
		return "", false
	}
	parts := strings.Split(clean, "/")
	if len(parts) <= 1 {
		return "", false
	}
	rel := strings.Join(parts[1:], "/")
	return rel, true
}

func safeArchivePath(root, child string) (string, error) {
	if filepath.IsAbs(child) {
		return "", errors.New("snapshot entry path is absolute")
	}
	clean := filepath.Clean(child)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot entry escapes repository")
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot entry escapes repository")
	}
	return target, nil
}

func parseBuildMetadata(data []byte) (string, error) {
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("parse build metadata: %w", err)
	}
	digest, _ := metadata["containerimage.digest"].(string)
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return "", errors.New("build metadata missing containerimage.digest")
	}
	return digest, nil
}

var pushFailureTokens = []string{
	"failed to push",
	"push failed",
	"error pushing",
	"unauthorized",
	"denied",
	"insufficient_scope",
	"authentication required",
	"requested access",
}

func classifyBuildctlFailure(req commandRequest, runErr error, output []byte) error {
	formatted := formatBuildCommandError(req, runErr, output)
	signal := strings.ToLower(runErr.Error() + "\n" + string(output))
	kind := failureKindBuild
	for _, token := range pushFailureTokens {
		if strings.Contains(signal, token) {
			kind = failureKindPush
			break
		}
	}
	return &buildFailureError{kind: kind, err: formatted}
}

func formatBuildCommandError(req commandRequest, err error, output []byte) error {
	message := fmt.Sprintf("%s %s: %v", req.Binary, strings.Join(req.Args, " "), err)
	tail := strings.TrimSpace(string(output))
	if tail == "" {
		return errors.New(message)
	}
	if len(tail) > maxBuildFailureTailBytes {
		tail = tail[len(tail)-maxBuildFailureTailBytes:]
	}
	return fmt.Errorf("%s: %s", message, tail)
}

func dockerDefaultConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".docker")
}

func symlinkIfExists(src, dst string) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
	}
	return nil
}

func runtimeDigestRef(pushRef, digest string) string {
	base := pushRef
	if tagSeparator := strings.LastIndexByte(pushRef, ':'); tagSeparator > strings.LastIndexByte(pushRef, '/') {
		base = pushRef[:tagSeparator]
	}
	return base + "@" + digest
}

func loadTransportCredentials(cfg config.InternalClientTLSConfig) (credentials.TransportCredentials, error) {
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca file: %w", err)
	}
	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read cert file: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 key pair: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append control plane CA: no certificates added")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   cfg.ServerName,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
