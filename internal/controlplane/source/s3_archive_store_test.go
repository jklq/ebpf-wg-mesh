package source

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestS3ArchiveStoreReadyProbesBucket(t *testing.T) {
	fake := newFakeS3(t, "ready-bucket")
	store, err := NewS3ArchiveStore(fake.config())
	if err != nil {
		t.Fatal(err)
	}
	if !store.Ready() {
		t.Fatal("available bucket reported unready")
	}
	fake.failRequests(http.MethodHead, 1)
	if store.Ready() {
		t.Fatal("unavailable bucket reported ready")
	}
}

func TestS3ArchiveStoreSendsSSEHeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("aes256", func(t *testing.T) {
		t.Parallel()
		fake := newFakeS3(t, "sse-bucket")
		cfg := fake.config()
		cfg.ServerSideEncryption = "AES256"
		store, err := NewS3ArchiveStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("sse-content")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.putSSE) != 1 || fake.putSSE[0] != "AES256" {
			t.Fatalf("SSE headers = %v", fake.putSSE)
		}
	})

	t.Run("kms with key id", func(t *testing.T) {
		t.Parallel()
		fake := newFakeS3(t, "sse-bucket")
		cfg := fake.config()
		cfg.ServerSideEncryption = "aws:kms"
		cfg.SSEKMSKeyID = "arn:aws:kms:us-east-1:1234:key/abcd"
		store, err := NewS3ArchiveStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("kms-content")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		obj, ok := fake.objects[key]
		if !ok || obj.kmsKey != "arn:aws:kms:us-east-1:1234:key/abcd" || obj.sse != "aws:kms" {
			t.Fatalf("stored SSE state = %+v", obj)
		}
	})
}

func TestS3ArchiveStoreAppliesPrefix(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "prefix-bucket")
	cfg := fake.config()
	cfg.Prefix = "tenant-a/archives"
	store, err := NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("prefixed")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	if got := fake.storedDigest("tenant-a/archives/" + key); got != digest {
		t.Fatalf("prefixed object digest = %q, want %q", got, digest)
	}
	if _, err := store.Stat(ctx, key); err != nil {
		t.Fatal(err)
	}
}

func TestS3ArchiveStoreRetriesTransientFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "retry-bucket")
	cfg := fake.config()
	cfg.MaxRetries = 3
	store, err := NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()
	payload := []byte("retry-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	fake.failRequests(http.MethodPut, 2)
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	fake.failRequests(http.MethodGet, 1)
	chunk, err := store.ReadRange(ctx, key, 0, len(payload))
	if err != nil || string(chunk) != string(payload) {
		t.Fatalf("range after retry = %q %v", chunk, err)
	}
}

func TestS3ArchiveStoreReportsTransientAfterRetries(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "retry-bucket")
	cfg := fake.config()
	cfg.MaxRetries = 2
	store, err := NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()
	fake.failRequests(http.MethodHead, 10)
	key, err := ArchiveObjectKey(ArchiveDigest([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Stat(ctx, key)
	if err == nil || !IsArchiveTransient(err) {
		t.Fatalf("stat error = %v, want transient", err)
	}
}

func TestS3ArchiveStoreRejectsRangeIgnoredByServer(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "range-bucket")
	fake.mu.Lock()
	fake.ignoreRange = true
	fake.mu.Unlock()
	store, err := NewS3ArchiveStore(fake.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("range-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRange(ctx, key, 4, 4); err == nil || !IsArchiveCorrupt(err) {
		t.Fatalf("range error = %v, want corrupt", err)
	}
}

func TestS3ArchiveStoreAuthenticatesFromCredentialsFile(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "auth-bucket")
	store, err := NewS3ArchiveStore(fake.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("auth-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.putAuth) != 1 || !strings.HasPrefix(fake.putAuth[0], "AWS4-HMAC-SHA256 Credential=test-access-key/") {
		t.Fatalf("Authorization headers = %v", fake.putAuth)
	}
}

func TestS3ArchiveStoreAnonymousWithoutCredentials(t *testing.T) {
	fake := newFakeS3(t, "auth-bucket")
	cfg := fake.config()
	cfg.CredentialsFile = ""
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	store, err := NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("anon-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.putAuth) != 1 || fake.putAuth[0] != "" {
		t.Fatalf("Authorization headers = %v, want anonymous", fake.putAuth)
	}
}

func TestS3ArchiveStoreUsesContainerCredentials(t *testing.T) {
	fake := newFakeS3(t, "auth-bucket")
	credsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"AccessKeyId":     "container-access",
			"SecretAccessKey": "container-secret",
			"Token":           "container-token",
			"Expiration":      "2034-05-24T12:00:00Z",
		})
	}))
	t.Cleanup(credsServer.Close)
	cfg := fake.config()
	cfg.CredentialsFile = ""
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", credsServer.URL)
	store, err := NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("workload-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.putAuth) != 1 || !strings.HasPrefix(fake.putAuth[0], "AWS4-HMAC-SHA256 Credential=container-access/") {
		t.Fatalf("Authorization headers = %v", fake.putAuth)
	}
}

func TestS3ArchiveStoreRejectsBadCredentialsFile(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "auth-bucket")
	cfg := fake.config()
	cfg.CredentialsFile = t.TempDir() + "/missing.json"
	if _, err := NewS3ArchiveStore(cfg); err == nil {
		t.Fatal("expected missing credentials file to fail")
	}
	badPath := t.TempDir() + "/bad.json"
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CredentialsFile = badPath
	if _, err := NewS3ArchiveStore(cfg); err == nil {
		t.Fatal("expected malformed credentials file to fail")
	}
}

func TestS3ArchiveStoreDetectsDigestMetadataMismatch(t *testing.T) {
	t.Parallel()
	fake := newFakeS3(t, "meta-bucket")
	store, err := NewS3ArchiveStore(fake.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("meta-content")
	digest := ArchiveDigest(payload)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.objects[key].digest = ArchiveDigest([]byte("tampered"))
	fake.mu.Unlock()
	if _, err := store.Stat(ctx, key); err == nil || !IsArchiveCorrupt(err) {
		t.Fatalf("stat error = %v, want corrupt", err)
	}
}

func TestNewSourceArchiveStoreDispatchesProviders(t *testing.T) {
	t.Parallel()
	fileStore, err := NewSourceArchiveStore(config.SourceArchiveConfig{
		Provider:  config.SourceArchiveProviderFile,
		Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fileStore.(*FileArchiveStore); !ok {
		t.Fatalf("provider file = %T", fileStore)
	}
	fake := newFakeS3(t, "dispatch-bucket")
	s3Store, err := NewSourceArchiveStore(config.SourceArchiveConfig{
		Provider: config.SourceArchiveProviderS3,
		S3:       fake.config(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s3Store.(*S3ArchiveStore); !ok {
		t.Fatalf("provider s3 = %T", s3Store)
	}
	if _, err := NewSourceArchiveStore(config.SourceArchiveConfig{Provider: "gcs"}); err == nil {
		t.Fatal("expected unknown provider to fail")
	}
}
