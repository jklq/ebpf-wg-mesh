package recovery

import (
	"net/url"
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

func TestNativeBackupMustUseInventoriedIndependentDestination(t *testing.T) {
	storage := StorageConfig{Bucket: "independent", Prefix: "production", Endpoint: "https://s3.recovery.example"}
	u, _ := url.Parse("s3://independent/production/database?AWS_ENDPOINT=https%3A%2F%2Fs3.recovery.example")
	if err := checkBackupDestination(u, storage, "production/database"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"s3://primary/production/database", "s3://independent/other/database", "s3://independent/production/database?AWS_ENDPOINT=https%3A%2F%2Fprimary.example"} {
		u, _ := url.Parse(raw)
		if err := checkBackupDestination(u, storage, "production/database"); err == nil {
			t.Fatal("native backup and protected inventory used different destinations")
		}
	}
}
