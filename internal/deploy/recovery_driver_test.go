package deploy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/recovery"
)

func TestRecoveryExecutableUploadStreamsChecksDigestAndInstallsAtomically(t *testing.T) {
	i, h, dir := localRecoverySSH(t)
	data := bytes.Repeat([]byte{0, 1, 2, 10, 255}, 1<<17)
	local := filepath.Join(dir, "protected.bin")
	if err := os.WriteFile(local, data, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "remote's files", "program")
	want := strings.TrimPrefix(recovery.Digest(data), "sha256:")
	if err := (SSHRemote{}).Upload(context.Background(), i, h, local, target, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("protected binary changed in transport", err)
	}
	if err := (SSHRemote{}).Upload(context.Background(), i, h, local, target, strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched protected digest installed")
	}
	got, _ = os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Fatal("failed upload replaced the installed binary")
	}
	if _, err := os.Stat(target + ".next"); !os.IsNotExist(err) {
		t.Fatal("failed upload left a partial executable")
	}
}

func localRecoverySSH(t *testing.T) (Installation, Host, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Fatal("test requires sha256sum or shasum")
		}
		if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nfor arg; do last=$arg; done\nexec /bin/sh -c \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	key := filepath.Join(dir, "private-key")
	if err := os.WriteFile(key, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	i := Installation{Secrets: map[string]SecretRef{"ssh": {File: key}}}
	h := Host{ID: "recovery", SSH: SSH{Address: "127.0.0.1", User: "root", Key: "ssh"}}
	return i, h, dir
}

type protectedExecutableFile struct {
	recovery.Storage
	content []byte
	reads   int
}

func (s *protectedExecutableFile) Get(_ context.Context, _ recovery.Object, target string) error {
	s.reads++
	return os.WriteFile(target, s.content, 0600)
}

func TestRecoveryExecutableReuseChecksActualBytesOnEveryRetry(t *testing.T) {
	i, h, root := localRecoverySSH(t)
	content := bytes.Repeat([]byte{0, 1, 10, 255}, 1024)
	digest := recovery.Digest(content)
	a := Artifact{SHA256: strings.TrimPrefix(digest, "sha256:")}
	target := filepath.Join(root, "program")
	if err := os.WriteFile(target, content, 0700); err != nil {
		t.Fatal(err)
	}
	s := &protectedExecutableFile{content: content}
	d := SSHDriver{Remote: SSHRemote{}, recoveryPoint: recovery.Point{Dependencies: []recovery.Dependency{{Kind: "tool", ID: "protected", Digest: digest, Objects: []recovery.Object{{Digest: digest, Size: int64(len(content))}}}}}, recoveryService: recovery.Service{Storage: s}}
	p := Plan{Installation: i}
	if err := d.uploadRecovered(context.Background(), p, h, "protected", a, target); err != nil || s.reads != 0 {
		t.Fatal("exact executable was not reused", err, s.reads)
	}
	if err := os.WriteFile(target, []byte("corrupt executable despite previous staging"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.uploadRecovered(context.Background(), p, h, "protected", a, target); err != nil || s.reads != 1 {
		t.Fatal("corrupt cache did not require protected content", err, s.reads)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, content) {
		t.Fatal("protected executable bytes were not installed", err)
	}
	// The effect succeeded before the interrupted installer recorded its marker.
	if err := d.uploadRecovered(context.Background(), p, h, "protected", a, target); err != nil || s.reads != 1 {
		t.Fatal("retry did not revalidate and retain installed bytes", err, s.reads)
	}
}
