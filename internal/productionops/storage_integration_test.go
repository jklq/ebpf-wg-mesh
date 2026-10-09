//go:build integration

package productionops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/recovery"
	iniFile "gopkg.in/ini.v1"
)

// An actual TLS MinIO process, isolated credentials, KMS key and on-disk bucket.
// No mock bucket receipts or object-version metadata are used.
func nativeObjectStore(t *testing.T, ctx context.Context) recovery.StorageConfig {
	return nativeObjectStoreAt(t, ctx, "127.0.0.8", true)
}

func nativeObjectStoreAt(t *testing.T, ctx context.Context, host string, immutable bool) recovery.StorageConfig {
	t.Helper()
	binary := os.Getenv("MINIO_BINARY")
	if binary == "" {
		binary = "/tmp/platform-native-inputs/minio"
	}
	mc := os.Getenv("MINIO_CLIENT_BINARY")
	if mc == "" {
		mc = "/tmp/platform-native-inputs/mc"
	}
	aws := os.Getenv("AWS_BINARY")
	if aws == "" {
		aws = "/tmp/platform-native-inputs/aws/dist/aws"
	}
	for _, file := range []string{binary, mc, aws} {
		if _, err := os.Stat(file); err != nil {
			t.Skip("native S3 integration requires MINIO_BINARY, MINIO_CLIENT_BINARY and AWS_BINARY")
		}
	}
	dir := t.TempDir()
	dataRoot := dir
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		dataRoot, err = os.MkdirTemp("/dev/shm", "platform-storage-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dataRoot) })
	}
	ca, err := certificate("isolated-storage-ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := certificate("node", &ca, []string{host})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"certs/public.crt": node.Certificate, "certs/private.key": node.Key, "ca.crt": ca.Certificate, "mc/certs/CAs/isolated.crt": ca.Certificate} {
		if err := writePrivate(filepath.Join(dir, name), b); err != nil {
			t.Fatal(err)
		}
	}
	random := make([]byte, 32)
	rand.Read(random)
	secret := "private-" + base64.RawURLEncoding.EncodeToString(random)
	rand.Read(random)
	kms := "isolated:" + base64.StdEncoding.EncodeToString(random)
	address := freeAddress(t, host)
	endpoint := "https://" + address
	log, err := os.Create(filepath.Join(dir, "minio.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	process := exec.CommandContext(ctx, binary, "server", "--address", address, "--console-address", freeAddress(t, host), "--certs-dir", filepath.Join(dir, "certs"), filepath.Join(dataRoot, "objects"))
	process.Env = append(os.Environ(), "MINIO_ROOT_USER=isolated-admin", "MINIO_ROOT_PASSWORD="+secret, "MINIO_KMS_SECRET_KEY="+kms, "MINIO_BROWSER=off")
	isolateChild(process)
	process.Stdout, process.Stderr = log, log
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Process.Kill(); process.Wait() })
	probe := Probe{URL: endpoint + "/minio/health/ready", CAFile: filepath.Join(dir, "ca.crt"), Status: http.StatusOK}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := probeHTTP(ctx, probe, false); err == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(log.Name())
			t.Fatalf("native storage startup: %s", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	privateValues := []string{secret}
	command := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, mc, append([]string{"--config-dir", filepath.Join(dir, "mc")}, args...)...)
		if body, err := cmd.CombinedOutput(); err != nil {
			message := string(body)
			for _, private := range privateValues {
				message = strings.ReplaceAll(message, private, "[redacted]")
			}
			t.Fatalf("native storage administration %s: %v: %s", strings.Join(args[:min(3, len(args))], " "), err, message)
		}
	}
	command("alias", "set", "isolated", endpoint, "isolated-admin", secret)
	if immutable {
		command("mb", "--with-lock", "isolated/recovery")
	} else {
		command("mb", "isolated/recovery")
		command("version", "enable", "isolated/recovery")
	}
	command("encrypt", "set", "sse-s3", "isolated/recovery")
	if immutable {
		command("retention", "set", "--default", "COMPLIANCE", "31d", "isolated/recovery")
	}
	rand.Read(random)
	writer := "private-" + base64.RawURLEncoding.EncodeToString(random)
	privateValues = append(privateValues, writer)
	command("admin", "user", "add", "isolated", "isolated-writer", writer)
	policy := map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Effect": "Allow", "Action": []string{"s3:*"}, "Resource": []string{"arn:aws:s3:::recovery", "arn:aws:s3:::recovery/*"}},
		map[string]any{"Effect": "Deny", "Action": []string{"s3:DeleteObjectVersion", "s3:PutBucketVersioning", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:BypassGovernanceRetention", "s3:DeleteBucket"}, "Resource": []string{"arn:aws:s3:::recovery", "arn:aws:s3:::recovery/*"}},
	}}
	if !immutable {
		policy["Statement"] = policy["Statement"].([]any)[:1]
	}
	p := filepath.Join(dir, "writer-policy.json")
	if err := saveJSON(p, policy); err != nil {
		t.Fatal(err)
	}
	command("admin", "policy", "create", "isolated", "recovery-writer", p)
	command("admin", "policy", "attach", "isolated", "recovery-writer", "--user", "isolated-writer")
	// The S3 resource policy and IAM policy both deny weakening recovery storage.
	statements := policy["Statement"].([]any)
	for _, v := range statements {
		v.(map[string]any)["Principal"] = map[string]string{"AWS": "isolated-writer"}
	}
	if err := saveJSON(p, policy); err != nil {
		t.Fatal(err)
	}
	command("anonymous", "set-json", p, "isolated/recovery")
	credentials := filepath.Join(dir, "writer.ini")
	if err := writePrivate(credentials, []byte("[writer]\naws_access_key_id=isolated-writer\naws_secret_access_key="+writer+"\n")); err != nil {
		t.Fatal(err)
	}
	c := recovery.StorageConfig{Endpoint: endpoint, CAFile: filepath.Join(dir, "ca.crt"), Bucket: "recovery", Prefix: "isolated", Region: "us-east-1", CredentialsFile: credentials, Profile: "writer", Account: "isolated-recovery", PrimaryAccount: "isolated-primary", FailureDomain: "isolated/offsite", PrimaryDomains: []string{"isolated/site"}, WriterPrincipal: "isolated-writer"}
	// Read the owner from the actual bucket, through the selected writer identity.
	cmd := exec.CommandContext(ctx, aws, "--no-cli-pager", "--output", "json", "--endpoint-url", endpoint, "--ca-bundle", c.CAFile, "--profile", "writer", "s3api", "get-bucket-acl", "--bucket", c.Bucket)
	cmd.Env = append(os.Environ(), "AWS_SHARED_CREDENTIALS_FILE="+credentials, "AWS_CONFIG_FILE=/dev/null", "AWS_EC2_METADATA_DISABLED=true")
	b, err := cmd.Output()
	if err != nil {
		t.Fatal("inspect native bucket owner", err)
	}
	var owner struct{ Owner struct{ ID string } }
	if err = json.Unmarshal(b, &owner); err != nil {
		t.Fatal(err)
	}
	c.Owner = owner.Owner.ID
	if c.Owner == "" {
		block, _ := pem.Decode(node.Certificate)
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		c.ServerPublicKeySHA256 = fmt.Sprintf("sha256:%x", sha256.Sum256(leaf.RawSubjectPublicKeyInfo))
	}
	return c
}

func TestNativeImmutableObjectProtectionAndIndependentKeyRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := nativeObjectStore(t, ctx)
	store := &recovery.S3{Config: c}
	if err := store.Check(ctx); err != nil {
		t.Fatal(err)
	}
	wrongIdentity := c
	wrongIdentity.ServerPublicKeySHA256 = "sha256:" + strings.Repeat("0", 64)
	if err := (&recovery.S3{Config: wrongIdentity}).Check(ctx); err == nil {
		t.Fatal("different endpoint authority accepted")
	}
	dir := t.TempDir()
	original := filepath.Join(dir, "original.keyring")
	plaintext := []byte(`{"version":1,"keys":{"k1":{"key":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) + `","algorithm":"AES-256-GCM"}}}`)
	if err := writePrivate(original, plaintext); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	service := recovery.Service{Storage: store, Prefix: c.Prefix, RecoveryKey: key}
	d, err := service.Protect(ctx, recovery.Requirement{Kind: "keyring", ID: "k1"}, original, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Both object version deletion and storage-policy weakening must be rejected
	// by the actual selected writer, independently of the local config declarations.
	if err := store.Delete(ctx, d.Objects[0]); err == nil {
		t.Fatal("production writer deleted an immutable version")
	}
	aws := os.Getenv("AWS_BINARY")
	if aws == "" {
		aws = "/tmp/platform-native-inputs/aws/dist/aws"
	}
	weak := exec.CommandContext(ctx, aws, "--no-cli-pager", "--endpoint-url", c.Endpoint, "--ca-bundle", c.CAFile, "--profile", c.Profile, "s3api", "put-bucket-versioning", "--bucket", c.Bucket, "--versioning-configuration", "Status=Suspended")
	weak.Env = append(os.Environ(), "AWS_SHARED_CREDENTIALS_FILE="+c.CredentialsFile, "AWS_CONFIG_FILE=/dev/null", "AWS_EC2_METADATA_DISABLED=true")
	if err := weak.Run(); err == nil {
		t.Fatal("production writer weakened bucket versioning")
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.keyring")
	if err := service.Materialize(ctx, d, restored, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(restored)
	if err != nil || string(b) != string(plaintext) {
		t.Fatal("independent materialization lost key material", err)
	}
	wrong := service
	wrong.RecoveryKey = []byte(strings.Repeat("x", 32))
	if err := wrong.Materialize(ctx, d, filepath.Join(dir, "wrong.keyring"), true); err == nil {
		t.Fatal("wrong independent key decrypted bundle")
	}
	inspected, err := store.Inspect(ctx, d.Objects[0])
	if err != nil || inspected.Version != d.Objects[0].Version || !inspected.RetainUntil.After(time.Now().Add(recovery.Retention)) {
		t.Fatal(fmt.Sprint("native exact-version retention", err))
	}
}

// Executes the reference fresh initializer, independent protection, native SQL
// scheduling/completion and recovery-file reconstruction. Full fleet admission
// and public endpoint recovery are separate fault exercises.
func TestNativeSourceArchiveTLSAndSelectedCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := nativeObjectStoreAt(t, ctx, "127.0.0.8", false)
	ini, err := iniFile.Load(mustRead(t, c.CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	sec := ini.Section(c.Profile)
	credentials := t.TempDir() + "/source.json"
	if err := saveJSON(credentials, map[string]string{"accessKeyId": sec.Key("aws_access_key_id").String(), "secretAccessKey": sec.Key("aws_secret_access_key").String()}); err != nil {
		t.Fatal(err)
	}
	cfg := config.SourceArchiveS3Config{Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: "archives", CredentialsFile: credentials, CAFile: c.CAFile, ServerSideEncryption: "AES256"}
	store, err := source.NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Ready() {
		t.Fatal("native source HEAD readiness failed")
	}
	data := "native source archive"
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(data)))
	key, err := source.ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, strings.NewReader(data), int64(len(data)), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	cfg.CAFile = ""
	untrusted, err := source.NewS3ArchiveStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.Ready() {
		t.Fatal("private S3 endpoint accepted without selected TLS authority")
	}
}
