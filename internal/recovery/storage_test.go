package recovery

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestProductionWriterDeniesRequireCorrectResourcesAndNoConditions(t *testing.T) {
	policy := map[string]any{"Statement": []any{
		map[string]any{"Effect": "Deny", "Principal": map[string]string{"AWS": "writer"}, "Action": []string{"s3:PutBucketVersioning", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:DeleteBucket"}, "Resource": "arn:aws:s3:::recovery"},
		map[string]any{"Effect": "Deny", "Principal": map[string]string{"AWS": "writer"}, "Action": []string{"s3:DeleteObjectVersion", "s3:BypassGovernanceRetention"}, "Resource": "arn:aws:s3:::recovery/*"},
	}}
	if err := checkWriterPolicy(string(jsonBytes(policy)), "writer", "recovery"); err != nil {
		t.Fatal(err)
	}
	statements := policy["Statement"].([]any)
	statement := statements[1].(map[string]any)
	statement["Resource"] = "arn:aws:s3:::recovery"
	if err := checkWriterPolicy(string(jsonBytes(policy)), "writer", "recovery"); err == nil {
		t.Fatal("bucket-only deny accepted for protected objects")
	}
	statement["Resource"] = "arn:aws:s3:::recovery/*"
	statement["Condition"] = map[string]any{"Bool": map[string]bool{"aws:SecureTransport": false}}
	if err := checkWriterPolicy(string(jsonBytes(policy)), "writer", "recovery"); err == nil {
		t.Fatal("conditional deletion deny accepted")
	}
	delete(statement, "Condition")
	statement["Principal"] = map[string]string{"AWS": "other"}
	if err := checkWriterPolicy(string(jsonBytes(policy)), "writer", "recovery"); err == nil {
		t.Fatal("different principal deletion deny accepted")
	}
}

func TestS3TimestampPrecisionCannotShortenRequiredRetention(t *testing.T) {
	until := time.Date(2026, 10, 1, 12, 0, 0, 999999999, time.FixedZone("local", 7200))
	encoded := s3RetentionTime(until)
	if encoded.Before(until) || encoded.Sub(until) > time.Second || encoded.Nanosecond() != 0 {
		t.Fatal("storage precision shortened required retention", encoded)
	}
}

func TestPgBackRestConfigBindsInventoriedIndependentDestination(t *testing.T) {
	dir := t.TempDir()
	credentials := dir + "/credentials"
	if err := writeTestPrivate(credentials, []byte("[recovery]\naws_access_key_id=access\naws_secret_access_key=secret\n")); err != nil {
		t.Fatal(err)
	}
	storage := StorageConfig{Bucket: "independent", Prefix: "production", Endpoint: "https://s3.recovery.example", Region: "us-east-1", Owner: "owner", CredentialsFile: credentials, Profile: "recovery", Account: "recovery", PrimaryAccount: "primary", FailureDomain: "independent", PrimaryDomains: []string{"primary"}, WriterPrincipal: "writer"}
	rendered, err := RenderPgBackRestConfig(storage, "production/database", "platform", "/var/lib/platform/data")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo1-type=s3", "repo1-path=/production/database", "repo1-s3-bucket=independent", "repo1-s3-endpoint=s3.recovery.example", "[platform]", "pg1-path=/var/lib/platform/data"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered pgBackRest config lacks %q:\n%s", want, rendered)
		}
	}
}

func writeTestPrivate(path string, b []byte) error {
	return os.WriteFile(path, b, 0600)
}
