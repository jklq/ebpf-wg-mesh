package recovery

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Storage pins reads and retention updates to exact object versions. Deletion is
// used only by independent recovery administration, never by platform retention.
type Storage interface {
	Check(context.Context) error
	Put(context.Context, string, string, time.Time) (Object, error)
	Get(context.Context, Object, string) error
	Inspect(context.Context, Object) (Object, error)
	Retain(context.Context, Object, time.Time) error
	Versions(context.Context, string) ([]Object, error)
	Delete(context.Context, Object) error
}

type StorageConfig struct {
	Binary          string   `json:"binary"`
	CAFile          string   `json:"caFile,omitempty"`
	Endpoint        string   `json:"endpoint"`
	Bucket          string   `json:"bucket"`
	Prefix          string   `json:"prefix"`
	Region          string   `json:"region"`
	Owner           string   `json:"owner"` // canonical owner returned by GetBucketAcl
	CredentialsFile string   `json:"credentialsFile"`
	Profile         string   `json:"profile"`
	Account         string   `json:"account"`
	PrimaryAccount  string   `json:"primaryAccount"`
	FailureDomain   string   `json:"failureDomain"`
	PrimaryDomains  []string `json:"primaryDomains"`
	WriterPrincipal string   `json:"writerPrincipal"`
}

func (c StorageConfig) Validate() error {
	if c.CAFile != "" && (!filepath.IsAbs(c.CAFile) || filepath.Clean(c.CAFile) != c.CAFile) {
		return fmt.Errorf("storage TLS CA file must be an absolute clean path")
	}
	if !filepath.IsAbs(c.Binary) || c.Bucket == "" || c.Region == "" || c.Owner == "" || !filepath.IsAbs(c.CredentialsFile) || c.Profile == "" || c.Account == "" || c.PrimaryAccount == "" || c.Account == c.PrimaryAccount || c.FailureDomain == "" || c.WriterPrincipal == "" {
		return fmt.Errorf("recovery storage requires a pinned S3 client, bucket owner, independent account/domain and explicit credentials")
	}
	if c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("recovery S3 endpoint requires HTTPS")
	}
	if strings.Trim(c.Prefix, "/") != c.Prefix || c.Prefix == "" || strings.Contains(c.Prefix, "..") {
		return fmt.Errorf("invalid recovery storage prefix")
	}
	for _, d := range c.PrimaryDomains {
		if d == c.FailureDomain {
			return fmt.Errorf("recovery storage shares primary failure domain %s", d)
		}
	}
	if len(c.PrimaryDomains) == 0 {
		return fmt.Errorf("primary site and storage failure domains must be declared")
	}
	return nil
}

// S3 uses the supported S3 operations in a release-pinned AWS CLI v2 executable.
// It works with compatible endpoints and never invokes a shell.
type S3 struct{ Config StorageConfig }

func (s *S3) call(ctx context.Context, result any, operation string, args ...string) error {
	c := s.Config
	argv := []string{"--no-cli-pager", "--output", "json", "--region", c.Region, "--profile", c.Profile}
	if c.Endpoint != "" {
		argv = append(argv, "--endpoint-url", c.Endpoint)
	}
	if c.CAFile != "" {
		argv = append(argv, "--ca-bundle", c.CAFile)
	}
	argv = append(argv, "s3api", operation, "--bucket", c.Bucket)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, c.Binary, argv...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AWS_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "AWS_SHARED_CREDENTIALS_FILE="+c.CredentialsFile, "AWS_CONFIG_FILE=/dev/null", "AWS_EC2_METADATA_DISABLED=true")
	// Do not expose command stderr: SDK diagnostics may contain credential URLs.
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("recovery S3 %s failed: %w", operation, err)
	}
	if result != nil && len(b) != 0 {
		if err := json.Unmarshal(b, result); err != nil {
			return fmt.Errorf("decode S3 %s response: %w", operation, err)
		}
	}
	return nil
}

func (s *S3) Check(ctx context.Context) error {
	if err := s.Config.Validate(); err != nil {
		return err
	}
	var owner struct{ Owner struct{ ID string } }
	if err := s.call(ctx, &owner, "get-bucket-acl"); err != nil {
		return err
	}
	if owner.Owner.ID != s.Config.Owner {
		return fmt.Errorf("recovery bucket owner differs from the declared independent owner")
	}
	var versioning struct{ Status string }
	if err := s.call(ctx, &versioning, "get-bucket-versioning"); err != nil {
		return err
	}
	if versioning.Status != "Enabled" {
		return fmt.Errorf("recovery bucket versioning must be enabled")
	}
	var encryption struct {
		ServerSideEncryptionConfiguration struct {
			Rules []struct{ ApplyServerSideEncryptionByDefault struct{ SSEAlgorithm string } }
		}
	}
	if err := s.call(ctx, &encryption, "get-bucket-encryption"); err != nil {
		return err
	}
	if len(encryption.ServerSideEncryptionConfiguration.Rules) == 0 || encryption.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm == "" {
		return fmt.Errorf("native database backup writes require bucket encryption at rest")
	}
	var lock struct {
		ObjectLockConfiguration struct {
			ObjectLockEnabled string
			Rule              struct {
				DefaultRetention struct {
					Mode  string
					Days  int
					Years int
				}
			}
		}
	}
	if err := s.call(ctx, &lock, "get-object-lock-configuration"); err != nil {
		return err
	}
	d := lock.ObjectLockConfiguration.Rule.DefaultRetention
	if lock.ObjectLockConfiguration.ObjectLockEnabled != "Enabled" || d.Mode != "COMPLIANCE" || (d.Days < 31 && d.Years < 1) {
		return fmt.Errorf("recovery bucket needs Object Lock COMPLIANCE default retention of at least 31 days for native backup writes")
	}
	var policy struct{ Policy string }
	if err := s.call(ctx, &policy, "get-bucket-policy"); err != nil {
		return err
	}
	return checkWriterPolicy(policy.Policy, s.Config.WriterPrincipal, s.Config.Bucket)
}

// Require unconditional resource-policy denies, rather than inferring credential
// scope from the fact that a harmless read succeeded. IAM grants cannot override
// these explicit denies on the production writer principal.
func checkWriterPolicy(raw, writer, bucket string) error {
	var p struct {
		Statement []struct {
			Effect    string
			Principal any
			Action    any
			Resource  any
			Condition map[string]any
		}
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return fmt.Errorf("invalid recovery bucket policy")
	}
	values := func(v any) []string {
		switch x := v.(type) {
		case string:
			return []string{x}
		case []any:
			var out []string
			for _, v := range x {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	denied := map[string]bool{}
	for _, statement := range p.Statement {
		if statement.Effect != "Deny" || len(statement.Condition) > 0 {
			continue
		}
		principal := statement.Principal
		if m, ok := principal.(map[string]any); ok {
			principal = m["AWS"]
		}
		applies := false
		for _, v := range values(principal) {
			if v == writer || v == "*" {
				applies = true
			}
		}
		if !applies {
			continue
		}
		for _, resource := range values(statement.Resource) {
			if resource != "*" && resource != "arn:aws:s3:::"+bucket && resource != "arn:aws:s3:::"+bucket+"/*" {
				continue
			}
			for _, action := range values(statement.Action) {
				denied[action+"/"+resource] = true
			}
		}
	}
	for _, action := range []string{"s3:DeleteObjectVersion", "s3:PutBucketVersioning", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:BypassGovernanceRetention", "s3:DeleteBucket"} {
		resource := "arn:aws:s3:::" + bucket
		if action == "s3:DeleteObjectVersion" || action == "s3:BypassGovernanceRetention" {
			resource += "/*"
		}
		if !denied[action+"/"+resource] && !denied[action+"/*"] && !denied["s3:*/"+resource] && !denied["*/*"] && !denied["s3:*/*"] {
			return fmt.Errorf("recovery bucket must explicitly deny production writer %s", action)
		}
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key, file string, until time.Time) (Object, error) {
	until = s3RetentionTime(until)
	digest, size, err := FileDigest(file)
	if err != nil {
		return Object{}, err
	}
	var result struct{ VersionId string }
	rawDigest, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if size > 5<<30 {
		result.VersionId, err = s.multipart(ctx, key, file, digest, size, until)
	} else {
		err = s.call(ctx, &result, "put-object", "--key", key, "--body", file, "--checksum-algorithm", "SHA256", "--checksum-sha256", base64.StdEncoding.EncodeToString(rawDigest), "--server-side-encryption", "AES256", "--metadata", "digest="+digest, "--object-lock-mode", "COMPLIANCE", "--object-lock-retain-until-date", until.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return Object{}, err
	}
	o := Object{Key: key, Version: result.VersionId, Digest: digest, Size: size, RetainUntil: until}
	if o.Version == "" || o.Version == "null" {
		return Object{}, fmt.Errorf("S3 upload did not return a protected object version")
	}
	return o, nil
}

func (s *S3) Get(ctx context.Context, o Object, file string) error {
	return s.call(ctx, nil, "get-object", "--key", o.Key, "--version-id", o.Version, file)
}
func (s *S3) Inspect(ctx context.Context, o Object) (Object, error) {
	var r struct {
		VersionId                 string
		ContentLength             int64
		Metadata                  map[string]string
		ObjectLockMode            string
		ObjectLockRetainUntilDate time.Time
		ServerSideEncryption      string
	}
	if err := s.call(ctx, &r, "head-object", "--key", o.Key, "--version-id", o.Version); err != nil {
		return Object{}, err
	}
	if r.VersionId != o.Version || r.ObjectLockMode != "COMPLIANCE" || r.ServerSideEncryption == "" {
		return Object{}, fmt.Errorf("object %s lacks version-pinned compliance retention or encryption", o.Key)
	}
	return Object{Key: o.Key, Version: o.Version, Digest: r.Metadata["digest"], Size: r.ContentLength, RetainUntil: r.ObjectLockRetainUntilDate}, nil
}
func (s *S3) Retain(ctx context.Context, o Object, until time.Time) error {
	until = s3RetentionTime(until)
	current, err := s.Inspect(ctx, o)
	if err != nil {
		return err
	}
	if !current.RetainUntil.Before(until) {
		return nil
	}
	retention := string(jsonBytes(map[string]any{"Mode": "COMPLIANCE", "RetainUntilDate": until.UTC().Format(time.RFC3339Nano)}))
	return s.call(ctx, nil, "put-object-retention", "--key", o.Key, "--version-id", o.Version, "--retention", retention)
}

// S3-compatible services may persist retention with second precision. Round up
// so serialization can never shorten the database timestamp's required horizon.
func s3RetentionTime(until time.Time) time.Time {
	second := until.UTC().Truncate(time.Second)
	if second.Before(until) {
		second = second.Add(time.Second)
	}
	return second
}
func (s *S3) Versions(ctx context.Context, prefix string) ([]Object, error) {
	var r struct {
		Versions []struct {
			Key       string
			VersionId string
			Size      int64
		}
	}
	// CLI pagination stays enabled; every page participates in the inventory.
	if err := s.call(ctx, &r, "list-object-versions", "--prefix", prefix); err != nil {
		return nil, err
	}
	var out []Object
	for _, v := range r.Versions {
		out = append(out, Object{Key: v.Key, Version: v.VersionId, Size: v.Size})
	}
	return out, nil
}
func (s *S3) Delete(ctx context.Context, o Object) error {
	return s.call(ctx, nil, "delete-object", "--key", o.Key, "--version-id", o.Version)
}
