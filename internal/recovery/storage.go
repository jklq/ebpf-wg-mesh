package recovery

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
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

// S3 uses a native, connection-reusing client. Configuration is immutable for its
// lifetime; renewed credentials create a new client at the next operations run.
type S3 struct {
	Config  StorageConfig
	once    sync.Once
	api     *s3.Client
	initErr error
}

func (s *S3) client() (*s3.Client, error) {
	s.once.Do(func() { s.api, s.initErr = storageClient(s.Config) })
	return s.api, s.initErr
}

// SDK errors may include credential-bearing request URLs and response bodies.
// Only the service error identifier and context cancellation leave this boundary.
func storageError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("recovery S3 %s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("recovery S3 %s: %w", operation, context.DeadlineExceeded)
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		return fmt.Errorf("recovery S3 %s failed (%s)", operation, api.ErrorCode())
	}
	return fmt.Errorf("recovery S3 %s failed", operation)
}

func (s *S3) Check(ctx context.Context) error {
	if err := s.Config.Validate(); err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bucket := aws.String(s.Config.Bucket)
	owner, err := c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: bucket})
	if err != nil {
		return storageError("bucket owner", err)
	}
	if s.Config.Owner != "" && (owner.Owner == nil || aws.ToString(owner.Owner.ID) != s.Config.Owner) {
		return fmt.Errorf("recovery bucket owner differs from the declared independent owner")
	}
	versioning, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: bucket})
	if err != nil {
		return storageError("bucket versioning", err)
	}
	if versioning.Status != types.BucketVersioningStatusEnabled {
		return fmt.Errorf("recovery bucket versioning must be enabled")
	}
	encryption, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	if err != nil {
		return storageError("bucket encryption", err)
	}
	if encryption.ServerSideEncryptionConfiguration == nil || len(encryption.ServerSideEncryptionConfiguration.Rules) == 0 || encryption.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault == nil || encryption.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm == "" {
		return fmt.Errorf("native database backup writes require bucket encryption at rest")
	}
	lock, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: bucket})
	if err != nil {
		return storageError("bucket retention", err)
	}
	if lock.ObjectLockConfiguration == nil || lock.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled || lock.ObjectLockConfiguration.Rule == nil || lock.ObjectLockConfiguration.Rule.DefaultRetention == nil {
		return fmt.Errorf("recovery bucket needs default compliance retention")
	}
	d := lock.ObjectLockConfiguration.Rule.DefaultRetention
	if d.Mode != types.ObjectLockRetentionModeCompliance || (aws.ToInt32(d.Days) < 31 && aws.ToInt32(d.Years) < 1) {
		return fmt.Errorf("recovery bucket needs Object Lock COMPLIANCE default retention of at least 31 days for native backup writes")
	}
	policy, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: bucket})
	if err != nil {
		return storageError("bucket policy", err)
	}
	return checkWriterPolicy(aws.ToString(policy.Policy), s.Config.WriterPrincipal, s.Config.Bucket)
}

func (s *S3) Put(ctx context.Context, key, file string, until time.Time) (Object, error) {
	until = s3RetentionTime(until)
	digest, size, err := FileDigest(file)
	if err != nil {
		return Object{}, err
	}
	c, err := s.client()
	if err != nil {
		return Object{}, err
	}
	var version string
	if size > 5<<30 {
		version, err = s.multipart(ctx, key, file, digest, size, until)
	} else {
		f, openErr := os.Open(file)
		if openErr != nil {
			return Object{}, openErr
		}
		defer f.Close()
		raw, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
		result, callErr := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(key), Body: f, ContentLength: aws.Int64(size), ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(raw)), ServerSideEncryption: types.ServerSideEncryptionAes256, Metadata: map[string]string{"digest": digest}, ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: &until})
		err = storageError("put object", callErr)
		if err == nil {
			version = aws.ToString(result.VersionId)
		}
	}
	if err != nil {
		return Object{}, err
	}
	if version == "" || version == "null" {
		return Object{}, fmt.Errorf("S3 upload did not return a protected object version")
	}
	return Object{Key: key, Version: version, Digest: digest, Size: size, RetainUntil: until}, nil
}

func (s *S3) Get(ctx context.Context, o Object, file string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(o.Key)}
	if o.Version != "" && o.Version != "null" {
		input.VersionId = aws.String(o.Version)
	}
	result, err := c.GetObject(ctx, input)
	if err != nil {
		return storageError("get object", err)
	}
	defer result.Body.Close()
	if input.VersionId != nil && aws.ToString(result.VersionId) != o.Version {
		return fmt.Errorf("S3 download returned a different version")
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, result.Body)
	if err == nil && result.ContentLength != nil && n != *result.ContentLength {
		err = fmt.Errorf("S3 download is incomplete")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return storageError("download content", err)
	}
	return closeErr
}
func (s *S3) Inspect(ctx context.Context, o Object) (Object, error) {
	c, err := s.client()
	if err != nil {
		return Object{}, err
	}
	r, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(o.Key), VersionId: aws.String(o.Version)})
	if err != nil {
		return Object{}, storageError("inspect object", err)
	}
	if aws.ToString(r.VersionId) != o.Version || r.ObjectLockMode != types.ObjectLockModeCompliance || r.ServerSideEncryption == "" || r.ObjectLockRetainUntilDate == nil {
		return Object{}, fmt.Errorf("object %s lacks version-pinned compliance retention or encryption", o.Key)
	}
	return Object{Key: o.Key, Version: o.Version, Digest: r.Metadata["digest"], Size: aws.ToInt64(r.ContentLength), RetainUntil: *r.ObjectLockRetainUntilDate}, nil
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
	c, err := s.client()
	if err != nil {
		return err
	}
	_, err = c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(o.Key), VersionId: aws.String(o.Version), Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: &until}})
	return storageError("extend retention", err)
}
func s3RetentionTime(until time.Time) time.Time {
	second := until.UTC().Truncate(time.Second)
	if second.Before(until) {
		second = second.Add(time.Second)
	}
	return second
}
func (s *S3) Versions(ctx context.Context, prefix string) ([]Object, error) {
	c, err := s.client()
	if err != nil {
		return nil, err
	}
	pages := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String(s.Config.Bucket), Prefix: aws.String(prefix)})
	var out []Object
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, storageError("list versions", err)
		}
		for _, v := range page.Versions {
			out = append(out, Object{Key: aws.ToString(v.Key), Version: aws.ToString(v.VersionId), Size: aws.ToInt64(v.Size)})
		}
	}
	return out, nil
}
func (s *S3) Delete(ctx context.Context, o Object) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	input := &s3.DeleteObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(o.Key)}
	if o.Version != "" && o.Version != "null" {
		input.VersionId = aws.String(o.Version)
	}
	_, err = c.DeleteObject(ctx, input)
	return storageError("delete object", err)
}
