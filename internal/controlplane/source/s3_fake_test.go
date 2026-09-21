package source

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ebof-wg-mesh/internal/config"
)

type fakeS3Object struct {
	body   []byte
	digest string
	size   int64
	sse    string
	kmsKey string
}

type fakeS3 struct {
	t      *testing.T
	server *httptest.Server
	bucket string

	mu          sync.Mutex
	objects     map[string]*fakeS3Object
	failNext    map[string]int
	failStatus  int
	ignoreRange bool
	putAuth     []string
	putSSE      []string
	putChecksum []string
}

func newFakeS3(t *testing.T, bucket string) *fakeS3 {
	t.Helper()
	fake := &fakeS3{
		t:          t,
		bucket:     bucket,
		objects:    make(map[string]*fakeS3Object),
		failNext:   make(map[string]int),
		failStatus: http.StatusServiceUnavailable,
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeS3) config() config.SourceArchiveS3Config {
	creds, err := json.Marshal(map[string]string{
		"access_key_id":     "test-access-key",
		"secret_access_key": "test-secret-key",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	path := f.t.TempDir() + "/s3-credentials.json"
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return config.SourceArchiveS3Config{
		Endpoint:              f.server.URL,
		Region:                "us-east-1",
		Bucket:                f.bucket,
		CredentialsFile:       path,
		RequestTimeoutSeconds: 10,
		MaxRetries:            3,
	}
}

func (f *fakeS3) failRequests(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[method] += n
}

func (f *fakeS3) objectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

func (f *fakeS3) storedDigest(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj, ok := f.objects[key]; ok {
		return obj.digest
	}
	return ""
}

func (f *fakeS3) corruptObject(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj, ok := f.objects[key]; ok && len(obj.body) > 0 {
		obj.body[0] ^= 0xff
	}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/"+f.bucket+"/")
	if !ok || key == "" {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<?xml version="1.0" ?><Error><Code>NoSuchBucket</Code></Error>`)
		return
	}
	f.mu.Lock()
	if f.failNext[r.Method] > 0 {
		f.failNext[r.Method]--
		status := f.failStatus
		f.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, `<?xml version="1.0" ?><Error><Code>ServiceUnavailable</Code></Error>`)
		return
	}
	f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		f.servePut(w, r, key)
	case http.MethodGet:
		f.serveGet(w, r, key)
	case http.MethodHead:
		f.serveHead(w, r, key)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) servePut(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	f.putAuth = append(f.putAuth, r.Header.Get("Authorization"))
	f.putSSE = append(f.putSSE, r.Header.Get("X-Amz-Server-Side-Encryption"))
	f.putChecksum = append(f.putChecksum, r.Header.Get("X-Amz-Checksum-Sha256"))
	if _, exists := f.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
		f.mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed)
		fmt.Fprint(w, `<?xml version="1.0" ?><Error><Code>PreconditionFailed</Code></Error>`)
		return
	}
	f.mu.Unlock()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxArchiveCompressedBytes+8))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	if want := base64.StdEncoding.EncodeToString(sum[:]); r.Header.Get("X-Amz-Checksum-Sha256") != want {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<?xml version="1.0" ?><Error><Code>BadDigest</Code></Error>`)
		return
	}
	size, _ := strconv.ParseInt(r.Header.Get("X-Amz-Meta-Size"), 10, 64)
	f.mu.Lock()
	f.objects[key] = &fakeS3Object{
		body:   body,
		digest: r.Header.Get("X-Amz-Meta-Digest"),
		size:   size,
		sse:    r.Header.Get("X-Amz-Server-Side-Encryption"),
		kmsKey: r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"),
	}
	f.mu.Unlock()
	w.Header().Set("ETag", `"fake-etag"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	ignoreRange := f.ignoreRange
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<?xml version="1.0" ?><Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
	if rangeHeader == "" || ignoreRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
		w.Header().Set("X-Amz-Meta-Digest", obj.digest)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.body)
		return
	}
	start, end, ok := parseRangeHeader(rangeHeader, int64(len(obj.body)))
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj.body)))
	w.Header().Set("X-Amz-Meta-Digest", obj.digest)
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(obj.body[start : end+1])
}

func (f *fakeS3) serveHead(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
	w.Header().Set("X-Amz-Meta-Digest", obj.digest)
	w.Header().Set("ETag", `"fake-etag"`)
	w.WriteHeader(http.StatusOK)
}

func parseRangeHeader(header string, size int64) (int64, int64, bool) {
	span, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, false
	}
	bounds := strings.SplitN(span, "-", 2)
	if len(bounds) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(bounds[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(strings.TrimSpace(bounds[1]), 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}
