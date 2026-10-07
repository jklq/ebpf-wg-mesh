package recovery

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func (s *S3) multipart(ctx context.Context, key, file, digest string, size int64, until time.Time) (string, error) {
	var created struct{ UploadId string }
	if err := s.call(ctx, &created, "create-multipart-upload", "--key", key, "--server-side-encryption", "AES256", "--metadata", "digest="+digest, "--checksum-algorithm", "SHA256", "--object-lock-mode", "COMPLIANCE", "--object-lock-retain-until-date", until.UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if created.UploadId == "" {
		return "", fmt.Errorf("S3 did not identify the multipart upload")
	}
	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.call(cleanup, nil, "abort-multipart-upload", "--key", key, "--upload-id", created.UploadId)
		}
	}()
	input, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer input.Close()
	partSize := max(int64(64<<20), (size+9999)/10000)
	type part struct {
		PartNumber     int
		ETag           string
		ChecksumSHA256 string
	}
	var parts []part
	for number := 1; ; number++ {
		f, err := temporary()
		if err != nil {
			return "", err
		}
		n, copyErr := io.CopyN(f, input, partSize)
		closeErr := f.Close()
		if n == 0 {
			os.Remove(f.Name())
			if copyErr == io.EOF {
				break
			}
			return "", copyErr
		}
		if copyErr != nil && copyErr != io.EOF {
			os.Remove(f.Name())
			return "", copyErr
		}
		if closeErr != nil {
			os.Remove(f.Name())
			return "", closeErr
		}
		digest, _, err := FileDigest(f.Name())
		if err != nil {
			os.Remove(f.Name())
			return "", err
		}
		raw, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
		var uploaded struct {
			ETag           string
			ChecksumSHA256 string
		}
		err = s.call(ctx, &uploaded, "upload-part", "--key", key, "--upload-id", created.UploadId, "--part-number", fmt.Sprint(number), "--body", f.Name(), "--checksum-sha256", base64.StdEncoding.EncodeToString(raw))
		os.Remove(f.Name())
		if err != nil {
			return "", err
		}
		if uploaded.ETag == "" || uploaded.ChecksumSHA256 != base64.StdEncoding.EncodeToString(raw) {
			return "", fmt.Errorf("S3 multipart part failed checksum verification")
		}
		parts = append(parts, part{number, uploaded.ETag, uploaded.ChecksumSHA256})
	}
	var result struct{ VersionId string }
	if err := s.call(ctx, &result, "complete-multipart-upload", "--key", key, "--upload-id", created.UploadId, "--multipart-upload", string(jsonBytes(map[string]any{"Parts": parts}))); err != nil {
		return "", err
	}
	completed = true
	return result.VersionId, nil
}
