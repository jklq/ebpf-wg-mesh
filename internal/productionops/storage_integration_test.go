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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
	iniFile "gopkg.in/ini.v1"
)

// An actual TLS MinIO process, isolated credentials, KMS key and on-disk bucket.
// No mock bucket receipts or object-version metadata are used.
func nativeObjectStore(t *testing.T, ctx context.Context) recovery.StorageConfig {
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
	node, err := certificate("node", &ca, []string{"127.0.0.8"})
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
	secret := base64.RawURLEncoding.EncodeToString(random)
	rand.Read(random)
	kms := "isolated:" + base64.StdEncoding.EncodeToString(random)
	address := freeAddress(t, "127.0.0.8")
	endpoint := "https://" + address
	log, err := os.Create(filepath.Join(dir, "minio.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	process := exec.CommandContext(ctx, binary, "server", "--address", address, "--console-address", freeAddress(t, "127.0.0.8"), "--certs-dir", filepath.Join(dir, "certs"), filepath.Join(dataRoot, "objects"))
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
	command := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, mc, append([]string{"--config-dir", filepath.Join(dir, "mc")}, args...)...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("native storage administration %s: %v", args[0], err)
		}
	}
	command("alias", "set", "isolated", endpoint, "isolated-admin", secret)
	command("mb", "--with-lock", "isolated/recovery")
	command("encrypt", "set", "sse-s3", "isolated/recovery")
	command("retention", "set", "--default", "COMPLIANCE", "31d", "isolated/recovery")
	rand.Read(random)
	writer := base64.RawURLEncoding.EncodeToString(random)
	command("admin", "user", "add", "isolated", "isolated-writer", writer)
	policy := map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Effect": "Allow", "Action": []string{"s3:*"}, "Resource": []string{"arn:aws:s3:::recovery", "arn:aws:s3:::recovery/*"}},
		map[string]any{"Effect": "Deny", "Action": []string{"s3:DeleteObjectVersion", "s3:PutBucketVersioning", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:BypassGovernanceRetention", "s3:DeleteBucket"}, "Resource": []string{"arn:aws:s3:::recovery", "arn:aws:s3:::recovery/*"}},
	}}
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
func TestNativeReferenceBootstrapCompletePointAndInstallerDiskLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	binary := os.Getenv("COCKROACH_BINARY")
	if binary == "" {
		binary = "/tmp/cockroach-v26.1.0"
	}
	releaseDir := os.Getenv("PRODUCTION_RELEASE_DIRECTORY")
	if releaseDir == "" {
		releaseDir = "/tmp/platform-release-r43"
	}
	for _, file := range []string{binary, filepath.Join(releaseDir, "release.json")} {
		if _, err := os.Stat(file); err != nil {
			t.Skip("requires COCKROACH_BINARY and a packaged PRODUCTION_RELEASE_DIRECTORY")
		}
	}
	storage := nativeObjectStore(t, ctx)
	r := testRunner(t)
	original := r.Config.StateDirectory
	r.Plan.CreatedAt = time.Now().UTC()
	monitor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var event map[string]any
		if req.Method != http.MethodPost || json.NewDecoder(req.Body).Decode(&event) != nil || event["installation"] != r.Plan.Installation.ID {
			http.Error(w, "invalid monitor event", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer monitor.Close()
	r.Plan.Installation.Backup.Monitor = monitor.URL
	r.HTTP = monitor.Client()
	r.Config.Version = 1
	r.Config.PlatformDomain = "example.invalid"
	r.Config.InternalServerName = "core.example.invalid"
	r.Config.Database.Binary = binary
	r.Plan.Installation.Hosts[0].Network.Address = "127.0.0.6"
	r.Config.Database.Address = freeAddress(t, "127.0.0.6")
	r.Config.Console = ConsoleConfig{Schema: "dashboard", TokenKeyFile: original + "/console.key", AdminBinary: releaseDir + "/console-admin-amd64"}
	r.Config.RecoveryConfig = original + "/recovery.json"
	r.Config.Database.BackupURIFile = original + "/backup-uri"
	r.Config.WildcardCertificate = original + "/wildcard.crt"
	r.Config.WildcardKey = original + "/wildcard.key"
	r.Config.SourceConfig = original + "/source-config.json"
	r.Plan.Installation.OperationsConfig = original + "/operations.json"
	r.Plan.Installation.Recovery.Inventory = original + "/inventory.json"
	// HTTPS artifacts are served from the actual packaged files. Their content
	// digests and ELF executables are unchanged; no placeholder tools are supplied.
	artifacts := httptest.NewTLSServer(http.FileServer(http.Dir(releaseDir)))
	defer artifacts.Close()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = artifacts.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	b, err := os.ReadFile(filepath.Join(releaseDir, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &r.Plan.Release); err != nil {
		t.Fatal(err)
	}
	for name, program := range r.Plan.Release.Programs {
		for architecture, a := range program.Artifacts {
			a.URL = artifacts.URL + "/" + string(name) + "-" + architecture
			program.Artifacts[architecture] = a
		}
		r.Plan.Release.Programs[name] = program
	}
	for name, architectures := range r.Plan.Release.Tools {
		for architecture, a := range architectures {
			a.URL = artifacts.URL + "/" + string(name) + "-" + architecture
			architectures[architecture] = a
		}
		r.Plan.Release.Tools[name] = architectures
	}
	r.Plan.Installation.Release = r.Plan.Release.ID
	rec := recovery.Config{Storage: storage, RecoveryKeyFile: original + "/recovery.key", KeyringFile: original + "/keyring.json", Images: recovery.Images{Binary: releaseDir + "/skopeo-amd64", AuthFile: original + "/registry-auth.json", CertificateDirectory: os.Getenv("PRODUCTION_REGISTRY_CERT_DIRECTORY")}, BackupConnection: "external://isolated_recovery", BackupPrefix: storage.Prefix + "/database", Installation: r.Plan.Installation.ID, Release: r.Plan.Release.ID, DatabaseURLFile: r.Config.Database.URLFile, ConsoleSchema: r.Config.Console.Schema}
	r.Plan.Installation.Backup.RecoveryKey = "independent-recovery-key"
	r.Plan.Installation.Secrets = map[string]deploy.SecretRef{"independent-recovery-key": {File: rec.RecoveryKeyFile}}
	key := make([]byte, 32)
	rand.Read(key)
	wildcardCA, err := certificate("wildcard-ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wildcard, err := certificate("node", &wildcardCA, []string{"*.example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{rec.RecoveryKeyFile: key, rec.Images.AuthFile: []byte(`{"auths":{}}`), r.Config.WildcardCertificate: wildcard.Certificate, r.Config.WildcardKey: wildcard.Key} {
		if err = writePrivate(path, data); err != nil {
			t.Fatal(err)
		}
	}
	if err = saveJSON(r.Config.SourceConfig, config.SourceArchiveConfig{Provider: "file", Directory: original + "/archives"}); err != nil {
		t.Fatal(err)
	}
	if err = saveJSON(r.Config.RecoveryConfig, rec); err != nil {
		t.Fatal(err)
	}
	if err = saveJSON(r.Plan.Installation.OperationsConfig, r.Config); err != nil {
		t.Fatal(err)
	}
	// The URI is a private input; production diagnostics never print its keys.
	ini, err := os.ReadFile(storage.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := iniFile.Load(ini)
	if err != nil {
		t.Fatal(err)
	}
	section := credentials.Section(storage.Profile)
	uri := url.URL{Scheme: "s3", Host: storage.Bucket, Path: "/" + rec.BackupPrefix}
	query := url.Values{"AWS_ACCESS_KEY_ID": {section.Key("aws_access_key_id").String()}, "AWS_SECRET_ACCESS_KEY": {section.Key("aws_secret_access_key").String()}, "AWS_REGION": {storage.Region}, "AWS_ENDPOINT": {storage.Endpoint}}
	uri.RawQuery = query.Encode()
	if err = writePrivate(r.Config.Database.BackupURIFile, []byte(uri.String())); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"recovery-protect", "database-credentials"} {
		if err = r.Execute(ctx, []string{hook}); err != nil {
			t.Fatal(hook, err)
		}
		t.Log("executed", hook)
		if _, err = r.Verify(ctx, []string{hook}); err != nil {
			t.Fatal(hook, err)
		}
	}
	pl := r.Plan.Placements[0]
	host := r.Plan.Installation.Hosts[0]
	remote := r.Remote.(isolatedRemote)
	log, err := os.Create(original + "/database.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	process := exec.CommandContext(ctx, binary, "start", "--certs-dir="+remote.path(host, cfgDir(r.Plan, pl)+"/certs"), "--store="+original+"/store", "--listen-addr="+r.Config.Database.Address, "--advertise-addr="+r.Config.Database.Address, "--http-addr="+freeAddress(t, "127.0.0.6"), "--join="+r.Config.Database.Address, "--cache=128MiB", "--max-sql-memory=128MiB")
	isolateChild(process)
	process.Stdout, process.Stderr = log, log
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			process.Process.Kill()
			process.Wait()
		}
	}()
	for deadline := time.Now().Add(20 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", r.Config.Database.Address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native database listener failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, hook := range []string{"database-init", "platform-bootstrap"} {
		if err = r.Execute(ctx, []string{hook}); err != nil {
			t.Fatal(hook, err)
		}
		t.Log("executed", hook)
		if _, err = r.Verify(ctx, []string{hook}); err != nil {
			t.Fatal(hook, err)
		}
	}
	db, err := r.db(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ca, err := os.ReadFile(storage.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "SET CLUSTER SETTING cloudstorage.http.custom_ca=$1", string(ca)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE EXTERNAL CONNECTION isolated_recovery AS '"+strings.ReplaceAll(uri.String(), "'", "''")+"'"); err != nil {
		t.Fatal("native external storage connection", err)
	}
	if _, err = r.nativeRecovery(ctx, "schedule"); err != nil {
		t.Fatal("native independent schedules", err)
	}

	if err = r.backup(ctx); err != nil {
		t.Fatal("complete native backup", err)
	}
	evidence, err := r.backupEvidence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("complete point metadata age measured %.2fs; this fixture does not establish a fleet RPO/RTO guarantee", time.Since(evidence.DataLossCutoff).Seconds())
	// Retain only the independently held recovery key, writer and native client.
	independentDir := t.TempDir()
	independent := rec
	independent.RecoveryKeyFile = independentDir + "/recovery.key"
	independent.KeyringFile = independentDir + "/unused-keyring"
	independent.DatabaseURLFile = ""
	if err = writePrivate(independent.RecoveryKeyFile, key); err != nil {
		t.Fatal(err)
	}
	independentConfig := independentDir + "/recovery.json"
	if err = saveJSON(independentConfig, independent); err != nil {
		t.Fatal(err)
	}
	operatorBinary := filepath.Join(independentDir, "operations")
	native, err := os.ReadFile(filepath.Join(releaseDir, "operations-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(operatorBinary, native, 0700); err != nil {
		t.Fatal(err)
	}
	process.Process.Kill()
	process.Wait()
	stopped = true
	db.Close()
	artifacts.Close()
	if err = os.RemoveAll(original); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	command := exec.CommandContext(ctx, operatorBinary, "recover-files", independentConfig, evidence.Backup, independentDir+"/recovered")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("packaged independent installer disk-loss recovery: %v: %s", err, output)
	}
	var result recoveredFiles
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal("packaged operations response", err)
	}

	stateKey, err := os.ReadFile(result.StateKey)
	if err != nil {
		t.Fatal(err)
	}
	store, err := deploy.OpenState(result.State, stateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Read()
	if err != nil || state.InstallationID != r.Plan.Installation.ID || state.Bundle.ID != r.Plan.Release.ID {
		t.Fatal("reconstructed installer state", err)
	}
	var recovered Config
	if err = privateJSON(result.OperationsConfig, &recovered); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(recovered.Console.TokenKeyFile); err != nil {
		t.Fatal("independently recovered console key", err)
	}
	if _, err = os.Stat(state.Policy.Recovery.Inventory); err != nil {
		t.Fatal("latest independent inventory missing", err)
	}
	diagnostic := exec.CommandContext(ctx, result.Tools[r.Plan.Release.ID+"/tool/aws/amd64"], "--version")
	diagnostic.Env = append(os.Environ(), "PLATFORM_RECOVERY_WORKSPACE="+independentDir+"/native-bundles")
	if err = diagnostic.Run(); err != nil {
		t.Fatal("independently recovered AWS executable", err)
	}
	t.Logf("independent installer/artifact/key recovery measured %.2fs", time.Since(began).Seconds())
}
