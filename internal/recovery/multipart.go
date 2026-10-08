package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (s *S3) multipart(ctx context.Context, key, file, digest string, size int64, until time.Time) (string, error) {
	c, err := s.client()
	if err != nil {
		return "", err
	}
	bucket, name := aws.String(s.Config.Bucket), aws.String(key)
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: name, ServerSideEncryption: types.ServerSideEncryptionAes256, Metadata: map[string]string{"digest": digest}, ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ChecksumType: types.ChecksumTypeComposite, ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: &until})
	if err != nil {
		return "", storageError("create multipart upload", err)
	}
	if aws.ToString(created.UploadId) == "" {
		return "", fmt.Errorf("S3 did not identify the multipart upload")
	}
	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, _ = c.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: bucket, Key: name, UploadId: created.UploadId})
		}
	}()
	input, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer input.Close()
	partSize := max(int64(64<<20), (size+9999)/10000)
	var parts []types.CompletedPart
	for offset, number := int64(0), int32(1); offset < size; offset, number = offset+partSize, number+1 {
		length := min(partSize, size-offset)
		h := sha256.New()
		if _, err = io.Copy(h, io.NewSectionReader(input, offset, length)); err != nil {
			return "", err
		}
		checksum := base64.StdEncoding.EncodeToString(h.Sum(nil))
		uploaded, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: bucket, Key: name, UploadId: created.UploadId, PartNumber: aws.Int32(number), Body: io.NewSectionReader(input, offset, length), ContentLength: aws.Int64(length), ChecksumSHA256: aws.String(checksum)})
		if err != nil {
			return "", storageError("upload part", err)
		}
		if aws.ToString(uploaded.ETag) == "" || aws.ToString(uploaded.ChecksumSHA256) != checksum {
			return "", fmt.Errorf("S3 multipart part failed checksum verification")
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: uploaded.ETag, ChecksumSHA256: uploaded.ChecksumSHA256})
	}
	result, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bucket, Key: name, UploadId: created.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, ChecksumType: types.ChecksumTypeComposite})
	if err != nil {
		return "", storageError("complete multipart upload", err)
	}
	completed = true
	return aws.ToString(result.VersionId), nil
}
