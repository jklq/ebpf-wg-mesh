package source

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestS3CanonicalQuerySortsEncodedPairs(t *testing.T) {
	query := url.Values{"z": {"1"}, "é": {"2"}, "a": {"~", " "}}
	if got, want := s3CanonicalQuery(query), "%C3%A9=2&a=%20&a=~&z=1"; got != want {
		t.Fatalf("canonical query = %q, want %q", got, want)
	}
}

// Golden SigV4 vectors cross-checked against botocore's SigV4Auth with frozen
// time 20240524T120000Z and the AWS documentation example credentials.
func TestSignS3RequestMatchesReferenceVectors(t *testing.T) {
	t.Parallel()

	ts := time.Date(2024, 5, 24, 12, 0, 0, 0, time.UTC)
	payload := "fe5554c35bea6327c4d064a20ce480a08b656290a7b6900cf598ac8be909c013"
	creds := s3Credentials{accessKey: "AKIDEXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	credsToken := s3Credentials{accessKey: "AKIDEXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", sessionToken: "session-token-123"}

	t.Run("put object", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequest(http.MethodPut, "https://s3.us-east-1.amazonaws.com/platform-source-archives/sha256/ab/abcdef0123456789.tgz", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Meta-Digest", "sha256:deadbeef")
		signS3Request(req, payload, creds, "us-east-1", ts)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20240524/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-meta-digest, Signature=0d1e5621fc6b724c411f58d1f21c0b8e6c3294e4ac1ee0ab2e34fe07c9a89089"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("get object", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequest(http.MethodGet, "https://s3.us-east-1.amazonaws.com/platform-source-archives/sha256/ab/abcdef0123456789.tgz", nil)
		if err != nil {
			t.Fatal(err)
		}
		signS3Request(req, emptyPayloadHash, creds, "us-east-1", ts)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20240524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34836c66806d6a4bc96278c0093ed38067bc9fdc5acd0d09ce14d87898264362"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("head with session token and kms", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequest(http.MethodHead, "https://minio.example.test:9000/bucket-with.dots/sha256/00/0011.tgz", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
		req.Header.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", "arn:aws:kms:us-east-1:1234:key/abcd")
		signS3Request(req, emptyPayloadHash, credsToken, "eu-west-1", ts)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20240524/eu-west-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id, Signature=83c6b8cfc8da6ca830968c6f0cc2f2026b1ac440e8df934a822fd3f62969fcc4"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization mismatch:\n got: %s\nwant: %s", got, want)
		}
	})
}

func TestSignS3RequestIgnoresUnsignedHeaders(t *testing.T) {
	t.Parallel()

	ts := time.Date(2024, 5, 24, 12, 0, 0, 0, time.UTC)
	creds := s3Credentials{accessKey: "AKIDEXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	sign := func(t *testing.T, extra map[string]string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://s3.us-east-1.amazonaws.com/bucket/sha256/ab/cd.tgz", nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range extra {
			req.Header.Set(key, value)
		}
		signS3Request(req, emptyPayloadHash, creds, "us-east-1", ts)
		return req.Header.Get("Authorization")
	}
	plain := sign(t, nil)
	withRange := sign(t, map[string]string{"Range": "bytes=0-9", "If-None-Match": "*"})
	if plain != withRange {
		t.Fatalf("unsigned headers changed the signature:\n%s\n%s", plain, withRange)
	}
}
