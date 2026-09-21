package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
)

type S3ArchiveStore struct {
	endpoint    *url.URL
	region      string
	bucket      string
	prefix      string
	sse         string
	kmsKeyID    string
	maxRetries  int
	credentials *s3CredentialsProvider
	httpClient  *http.Client
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

var _ ArchiveStore = (*S3ArchiveStore)(nil)

func NewS3ArchiveStore(cfg config.SourceArchiveS3Config) (*S3ArchiveStore, error) {
	endpoint, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("S3 endpoint %q must be an absolute URL", cfg.Endpoint)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("S3 endpoint %q must use http or https", cfg.Endpoint)
	}
	region := strings.TrimSpace(cfg.Region)
	bucket := strings.TrimSpace(cfg.Bucket)
	if region == "" || bucket == "" {
		return nil, errors.New("S3 region and bucket are required")
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	sse := strings.TrimSpace(cfg.ServerSideEncryption)
	switch sse {
	case "", "AES256", "aws:kms":
	default:
		return nil, fmt.Errorf("S3 server-side encryption %q must be empty, AES256, or aws:kms", cfg.ServerSideEncryption)
	}
	kmsKeyID := strings.TrimSpace(cfg.SSEKMSKeyID)
	if sse == "AES256" && kmsKeyID != "" {
		return nil, errors.New("S3 KMS key id requires aws:kms encryption")
	}
	timeout := cfg.RequestTimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	credentials := newS3CredentialsProvider(cfg.CredentialsFile)
	if err := credentials.validate(); err != nil {
		return nil, err
	}
	return &S3ArchiveStore{
		endpoint:    endpoint,
		region:      region,
		bucket:      bucket,
		prefix:      prefix,
		sse:         sse,
		kmsKeyID:    kmsKeyID,
		maxRetries:  maxRetries,
		credentials: credentials,
		httpClient:  &http.Client{Timeout: time.Duration(timeout) * time.Second},
		now:         time.Now,
		sleep: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}, nil
}

func (s *S3ArchiveStore) Ready() bool {
	return s != nil && s.endpoint != nil && s.region != "" && s.bucket != ""
}

func (s *S3ArchiveStore) objectPath(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid source archive object key %q", key)
	}
	if _, err := DigestFromObjectKey(key); err != nil {
		return "", err
	}
	full := key
	if s.prefix != "" {
		full = s.prefix + "/" + key
	}
	return "/" + s.bucket + "/" + full, nil
}

func (s *S3ArchiveStore) objectURL(key string) (*url.URL, error) {
	path, err := s.objectPath(key)
	if err != nil {
		return nil, err
	}
	out := *s.endpoint
	out.Path = strings.TrimSuffix(s.endpoint.Path, "/") + path
	out.RawPath = ""
	out.RawQuery = ""
	return &out, nil
}

func (s *S3ArchiveStore) Put(ctx context.Context, key string, body io.Reader, size int64, digest string) error {
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
	staged, err := os.CreateTemp("", "source-archive-upload-*")
	if err != nil {
		return fmt.Errorf("stage source archive upload: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(staged, hash), body, size+1)
	if err != nil && !errors.Is(err, io.EOF) {
		staged.Close()
		return fmt.Errorf("stage source archive upload: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		staged.Close()
		return ctxErr
	}
	if written != size {
		staged.Close()
		return fmt.Errorf("%w: declared size %d but body yielded %d bytes", ErrArchiveCorrupt, size, written)
	}
	if extra, err := io.Copy(io.Discard, io.LimitReader(body, 1)); err != nil {
		staged.Close()
		return fmt.Errorf("verify source archive body length: %w", err)
	} else if extra != 0 {
		staged.Close()
		return fmt.Errorf("%w: declared size %d but body is longer", ErrArchiveCorrupt, size)
	}
	sum := hash.Sum(nil)
	if actual := "sha256:" + fmt.Sprintf("%x", sum); actual != digest {
		staged.Close()
		return fmt.Errorf("%w: digest verification failed", ErrArchiveCorrupt)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		staged.Close()
		return fmt.Errorf("stage source archive upload: %w", err)
	}
	checksum := base64.StdEncoding.EncodeToString(sum)
	payloadHash := fmt.Sprintf("%x", sum)
	staged.Close()

	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.backoff(ctx, attempt); err != nil {
				return err
			}
		}
		body, err := os.Open(stagedPath)
		if err != nil {
			return err
		}
		done, attemptErr := s.putAttempt(ctx, key, body, size, digest, checksum, payloadHash)
		_ = body.Close()
		if done {
			return attemptErr
		}
		lastErr = attemptErr
	}
	return lastErr
}

func (s *S3ArchiveStore) putAttempt(ctx context.Context, key string, body io.Reader, size int64, digest, checksum, payloadHash string) (bool, error) {
	objectURL, err := s.objectURL(key)
	if err != nil {
		return true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, objectURL.String(), body)
	if err != nil {
		return true, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	req.Header.Set("X-Amz-Checksum-Sha256", checksum)
	req.Header.Set("X-Amz-Meta-Digest", digest)
	req.Header.Set("X-Amz-Meta-Size", strconv.FormatInt(size, 10))
	req.Header.Set("If-None-Match", "*")
	s.setSSEHeaders(req)
	if err := s.authorize(ctx, req, payloadHash); err != nil {
		return true, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return false, s.transientError("put source archive object", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return true, nil
	case resp.StatusCode == http.StatusPreconditionFailed:
		return true, s.convergeOnDuplicate(ctx, key, size, digest)
	case resp.StatusCode == http.StatusNotFound:
		return true, fmt.Errorf("put source archive object: bucket %q not found", s.bucket)
	case resp.StatusCode == http.StatusBadRequest:
		return true, fmt.Errorf("%w: put source archive object rejected the upload", ErrArchiveCorrupt)
	case isRetryableStatus(resp.StatusCode):
		return false, s.transientError("put source archive object", fmt.Errorf("status %d", resp.StatusCode))
	default:
		return true, fmt.Errorf("put source archive object: status %d", resp.StatusCode)
	}
}

func (s *S3ArchiveStore) convergeOnDuplicate(ctx context.Context, key string, size int64, digest string) error {
	stat, err := s.Stat(ctx, key)
	if err != nil {
		return err
	}
	if stat.Size != size || stat.Digest != digest {
		return fmt.Errorf("%w: existing object does not match digest", ErrArchiveCorrupt)
	}
	return nil
}

func (s *S3ArchiveStore) Open(ctx context.Context, key string) (io.ReadCloser, ObjectMetadata, error) {
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.backoff(ctx, attempt); err != nil {
				return nil, ObjectMetadata{}, err
			}
		}
		body, err := s.get(ctx, key)
		if err == nil {
			return &transientReadCloser{body: body}, meta, nil
		}
		if !IsArchiveTransient(err) {
			return nil, ObjectMetadata{}, err
		}
		lastErr = err
	}
	return nil, ObjectMetadata{}, lastErr
}

func (s *S3ArchiveStore) ReadRange(ctx context.Context, key string, offset int64, limit int) ([]byte, error) {
	if offset < 0 || limit <= 0 {
		return nil, errors.New("invalid source archive range")
	}
	end := offset + int64(limit) - 1
	rangeHeader := "bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(end, 10)
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.backoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		chunk, retry, err := s.rangeAttempt(ctx, key, rangeHeader, limit)
		if err == nil {
			return chunk, nil
		}
		if !retry {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *S3ArchiveStore) rangeAttempt(ctx context.Context, key, rangeHeader string, limit int) ([]byte, bool, error) {
	body, retry, err := s.getBody(ctx, key, rangeHeader)
	if err != nil {
		return nil, retry, err
	}
	defer body.Close()
	chunk, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, true, s.transientError("read source archive range", err)
	}
	if len(chunk) > limit {
		return nil, false, fmt.Errorf("%w: range read exceeded requested limit", ErrArchiveCorrupt)
	}
	return chunk, false, nil
}

func (s *S3ArchiveStore) get(ctx context.Context, key string) (io.ReadCloser, error) {
	body, _, err := s.getBody(ctx, key, "")
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (s *S3ArchiveStore) getBody(ctx context.Context, key, rangeHeader string) (io.ReadCloser, bool, error) {
	objectURL, err := s.objectURL(key)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL.String(), nil)
	if err != nil {
		return nil, false, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	if err := s.authorize(ctx, req, emptyPayloadHash); err != nil {
		return nil, false, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, true, s.transientError("get source archive object", err)
	}
	if rangeHeader != "" && resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, false, fmt.Errorf("%w: range request returned a full response", ErrArchiveCorrupt)
	}
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent:
		return resp.Body, false, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return io.NopCloser(bytes.NewReader(nil)), false, nil
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, false, fmt.Errorf("%w: %s", ErrArchiveNotFound, key)
	case isRetryableStatus(resp.StatusCode):
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, true, s.transientError("get source archive object", fmt.Errorf("status %d", resp.StatusCode))
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, false, fmt.Errorf("get source archive object: status %d", resp.StatusCode)
	}
}

func (s *S3ArchiveStore) Stat(ctx context.Context, key string) (ObjectMetadata, error) {
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.backoff(ctx, attempt); err != nil {
				return ObjectMetadata{}, err
			}
		}
		meta, retry, err := s.statAttempt(ctx, key)
		if err == nil {
			return meta, nil
		}
		if !retry {
			return ObjectMetadata{}, err
		}
		lastErr = err
	}
	return ObjectMetadata{}, lastErr
}

func (s *S3ArchiveStore) statAttempt(ctx context.Context, key string) (ObjectMetadata, bool, error) {
	objectURL, err := s.objectURL(key)
	if err != nil {
		return ObjectMetadata{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, objectURL.String(), nil)
	if err != nil {
		return ObjectMetadata{}, false, err
	}
	if err := s.authorize(ctx, req, emptyPayloadHash); err != nil {
		return ObjectMetadata{}, false, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ObjectMetadata{}, true, s.transientError("stat source archive object", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		size, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
		if err != nil || size < 0 {
			return ObjectMetadata{}, false, fmt.Errorf("%w: object size header is invalid", ErrArchiveCorrupt)
		}
		digest := strings.TrimSpace(resp.Header.Get("X-Amz-Meta-Digest"))
		if digest == "" {
			digest, err = DigestFromObjectKey(key)
			if err != nil {
				return ObjectMetadata{}, false, err
			}
		} else if expected, err := DigestFromObjectKey(key); err != nil || !strings.EqualFold(strings.TrimSpace(digest), expected) {
			return ObjectMetadata{}, false, fmt.Errorf("%w: stored digest metadata does not match object key", ErrArchiveCorrupt)
		} else {
			digest = expected
		}
		return ObjectMetadata{Size: size, Digest: digest}, false, nil
	case resp.StatusCode == http.StatusNotFound:
		return ObjectMetadata{}, false, fmt.Errorf("%w: %s", ErrArchiveNotFound, key)
	case isRetryableStatus(resp.StatusCode):
		return ObjectMetadata{}, true, s.transientError("stat source archive object", fmt.Errorf("status %d", resp.StatusCode))
	default:
		return ObjectMetadata{}, false, fmt.Errorf("stat source archive object: status %d", resp.StatusCode)
	}
}

func (s *S3ArchiveStore) Delete(ctx context.Context, key string) error {
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.backoff(ctx, attempt); err != nil {
				return err
			}
		}
		retry, err := s.deleteAttempt(ctx, key)
		if err == nil {
			return nil
		}
		if !retry {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (s *S3ArchiveStore) deleteAttempt(ctx context.Context, key string) (bool, error) {
	objectURL, err := s.objectURL(key)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, objectURL.String(), nil)
	if err != nil {
		return false, err
	}
	if err := s.authorize(ctx, req, emptyPayloadHash); err != nil {
		return false, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return true, s.transientError("delete source archive object", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusCreated,
		resp.StatusCode == http.StatusAccepted, resp.StatusCode == http.StatusNoContent,
		resp.StatusCode == http.StatusNotFound:
		return false, nil
	case isRetryableStatus(resp.StatusCode):
		return true, s.transientError("delete source archive object", fmt.Errorf("status %d", resp.StatusCode))
	default:
		return false, fmt.Errorf("delete source archive object: status %d", resp.StatusCode)
	}
}

func (s *S3ArchiveStore) setSSEHeaders(req *http.Request) {
	switch s.sse {
	case "AES256":
		req.Header.Set("X-Amz-Server-Side-Encryption", "AES256")
	case "aws:kms":
		req.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
		if s.kmsKeyID != "" {
			req.Header.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", s.kmsKeyID)
		}
	}
}

func (s *S3ArchiveStore) authorize(ctx context.Context, req *http.Request, payloadHash string) error {
	creds, anonymous, err := s.credentials.get(ctx)
	if err != nil {
		return err
	}
	if anonymous {
		return nil
	}
	signS3Request(req, payloadHash, creds, s.region, s.now().UTC())
	return nil
}

func (s *S3ArchiveStore) transientError(op string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrArchiveTransient, op, err)
}

func (s *S3ArchiveStore) backoff(ctx context.Context, attempt int) error {
	delay := 100 * time.Millisecond
	for i := 1; i < attempt && delay < 5*time.Second; i++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(delay)/2 + 1))
	return s.sleep(ctx, delay/2+jitter)
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return status == 0
	}
}

func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

type transientReadCloser struct {
	body io.ReadCloser
}

func (r *transientReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		if !IsArchiveTransient(err) && isNetworkError(err) {
			return n, fmt.Errorf("%w: read source archive object: %v", ErrArchiveTransient, err)
		}
	}
	return n, err
}

func (r *transientReadCloser) Close() error {
	return r.body.Close()
}
