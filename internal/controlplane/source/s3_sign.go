package source

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func s3HMAC(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func s3SigningKey(secret, date, region, service string) []byte {
	kDate := s3HMAC([]byte("AWS4"+secret), []byte(date))
	kRegion := s3HMAC(kDate, []byte(region))
	kService := s3HMAC(kRegion, []byte(service))
	return s3HMAC(kService, []byte("aws4_request"))
}

func s3Escape(value string, encodeSlash bool) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' || (c == '/' && !encodeSlash) {
			out.WriteByte(c)
			continue
		}
		fmt.Fprintf(&out, "%%%02X", c)
	}
	return out.String()
}

func s3CanonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return s3Escape(path, false)
}

func s3CanonicalQuery(raw url.Values) string {
	if len(raw) == 0 {
		return ""
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		values := append([]string(nil), raw[key]...)
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, s3Escape(key, true)+"="+s3Escape(value, true))
		}
	}
	return strings.Join(parts, "&")
}

func s3CanonicalHeaders(req *http.Request) (canonical string, signed string) {
	lowered := make(map[string][]string)
	for name, values := range req.Header {
		lowered[strings.ToLower(strings.TrimSpace(name))] = values
	}
	if _, ok := lowered["host"]; !ok {
		host := req.Host
		if host == "" && req.URL != nil {
			host = req.URL.Host
		}
		lowered["host"] = []string{host}
	}
	var names []string
	for name := range lowered {
		if name == "host" || strings.HasPrefix(name, "x-amz-") || name == "content-type" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		values := lowered[name]
		trimmed := make([]string, 0, len(values))
		for _, value := range values {
			trimmed = append(trimmed, strings.TrimSpace(value))
		}
		lines = append(lines, name+":"+strings.Join(trimmed, ","))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(names, ";")
}

func s3CanonicalRequest(req *http.Request, payloadHash string) string {
	query := url.Values{}
	if req.URL != nil {
		query = req.URL.Query()
	}
	path := "/"
	if req.URL != nil {
		path = req.URL.Path
		if req.URL.RawPath != "" {
			if decoded, err := url.PathUnescape(req.URL.RawPath); err == nil {
				path = decoded
			}
		}
		if path == "" {
			path = "/"
		}
	}
	canonicalHeaders, signedHeaders := s3CanonicalHeaders(req)
	return strings.Join([]string{
		req.Method,
		s3CanonicalURI(path),
		s3CanonicalQuery(query),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
}

func s3StringToSign(canonicalRequest, timestamp, date, region, service string) (string, string) {
	sum := sha256.Sum256([]byte(canonicalRequest))
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, region, service)
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n"), scope
}

func signS3Request(req *http.Request, payloadHash string, creds s3Credentials, region string, now time.Time) {
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	timestamp := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", timestamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.sessionToken)
	}
	canonical := s3CanonicalRequest(req, payloadHash)
	date := now.UTC().Format("20060102")
	toSign, scope := s3StringToSign(canonical, timestamp, date, region, "s3")
	_, signedHeaders := s3CanonicalHeaders(req)
	signature := hex.EncodeToString(s3HMAC(s3SigningKey(creds.secretKey, date, region, "s3"), []byte(toSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.accessKey, scope, signedHeaders, signature))
}
