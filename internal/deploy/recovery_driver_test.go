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
