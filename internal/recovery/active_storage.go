package recovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// VerifyActiveStore inspects the mutable active service using a unique probe
// object. It never deletes platform or workload data.
func (s *S3) VerifyActiveStore(ctx context.Context) error {
	config := s.Config
	if !filepath.IsAbs(config.CredentialsFile) || config.Bucket == "" || config.Region == "" || (config.Owner == "" && config.ServerPublicKeySHA256 == "") || config.Profile == "" {
		return fmt.Errorf("active S3 needs explicit credentials, owner and TLS service")
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	owner, err := c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String(config.Bucket)})
	if err != nil {
		return storageError("active owner", err)
	}
	if config.Owner != "" && (owner.Owner == nil || aws.ToString(owner.Owner.ID) != config.Owner) {
		return fmt.Errorf("active storage owner differs")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := strings.Trim(config.Prefix, "/") + "/.platform-inspection/" + hex.EncodeToString(nonce)
	result, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String(key), Body: bytes.NewReader(nonce), ContentLength: aws.Int64(int64(len(nonce))), ServerSideEncryption: types.ServerSideEncryptionAes256})
	if err != nil {
		return storageError("active write", err)
	}
	object := Object{Key: key, Version: aws.ToString(result.VersionId)}
	removed := false
	defer func() {
		if !removed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.Delete(cleanup, object)
		}
	}()
	f, err := temporary()
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	if err = s.Get(ctx, object, f.Name()); err != nil {
		return err
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if Digest(b) != Digest(nonce) {
		return fmt.Errorf("active S3 content readback differs")
	}
	if err := s.Delete(ctx, object); err != nil {
		return err
	}
	removed = true
	return nil
}
