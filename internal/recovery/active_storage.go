package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VerifyActiveStore inspects a mutable active storage service. Its capabilities
// differ from the independent immutable recovery bucket: normal retention is
// allowed to delete active data once it has been independently protected.
func (s *S3) VerifyActiveStore(ctx context.Context) error {
	c := s.Config
	if !filepath.IsAbs(c.Binary) || !filepath.IsAbs(c.CredentialsFile) || c.Bucket == "" || c.Region == "" || c.Owner == "" || c.Profile == "" || (c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "https://")) {
		return fmt.Errorf("active S3 needs explicit credentials, owner and TLS service")
	}
	var owner struct{ Owner struct{ ID string } }
	if err := s.call(ctx, &owner, "get-bucket-acl"); err != nil {
		return err
	}
	if owner.Owner.ID != c.Owner {
		return fmt.Errorf("active storage owner differs")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	f, err := os.CreateTemp("", "active-storage-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(nonce); err != nil {
		f.Close()
		return err
	}
	f.Close()
	key := strings.Trim(c.Prefix, "/") + "/.platform-inspection/" + hex.EncodeToString(nonce)
	var result struct{ VersionId string }
	if err = s.call(ctx, &result, "put-object", "--key", key, "--body", f.Name(), "--server-side-encryption", "AES256"); err != nil {
		return err
	}
	args := []string{"--key", key}
	if result.VersionId != "" && result.VersionId != "null" {
		args = append(args, "--version-id", result.VersionId)
	}
	defer s.call(context.WithoutCancel(ctx), nil, "delete-object", args...)
	if err = s.call(ctx, nil, "get-object", append(args, f.Name())...); err != nil {
		return err
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if Digest(b) != Digest(nonce) {
		return fmt.Errorf("active S3 content readback differs")
	}
	return nil
}
