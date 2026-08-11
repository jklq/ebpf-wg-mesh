package localteststack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvFileMissingIsNoop(t *testing.T) {
	t.Parallel()

	n, err := LoadDotEnvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if err != nil {
		t.Fatalf("LoadDotEnvFile: %v", err)
	}
	if n != 0 {
		t.Fatalf("set count = %d, want 0", n)
	}
}

func TestLoadDotEnvFileNamedPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := unixMkfifo(path); err != nil {
		t.Skipf("mkfifo not available: %v", err)
	}

	content := "PIPE_DOTENV_TEST=from-fifo\n"
	errCh := make(chan error, 1)
	go func() {
		// Writer side of the FIFO (1Password-style mount).
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			errCh <- err
			return
		}
		_, err = f.WriteString(content)
		_ = f.Close()
		errCh <- err
	}()

	_ = os.Unsetenv("PIPE_DOTENV_TEST")
	n, err := LoadDotEnvFile(path)
	if err != nil {
		t.Fatalf("LoadDotEnvFile: %v", err)
	}
	if n != 1 {
		t.Fatalf("set count = %d, want 1", n)
	}
	if got := os.Getenv("PIPE_DOTENV_TEST"); got != "from-fifo" {
		t.Fatalf("PIPE_DOTENV_TEST = %q", got)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("fifo writer: %v", err)
	}
}

func TestLoadDotEnvFileSetsMissingOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "" +
		"# comment\n" +
		"\n" +
		"DOTENV_TEST_A=from-file\n" +
		"export DOTENV_TEST_B=\"quoted value\"\n" +
		"DOTENV_TEST_C=unquoted # trailing comment\n" +
		"DOTENV_TEST_EXISTING=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	t.Setenv("DOTENV_TEST_EXISTING", "from-shell")
	// Ensure clean slate for the others.
	for _, key := range []string{"DOTENV_TEST_A", "DOTENV_TEST_B", "DOTENV_TEST_C"} {
		_ = os.Unsetenv(key)
	}

	n, err := LoadDotEnvFile(path)
	if err != nil {
		t.Fatalf("LoadDotEnvFile: %v", err)
	}
	if n != 3 {
		t.Fatalf("set count = %d, want 3", n)
	}
	if got := os.Getenv("DOTENV_TEST_A"); got != "from-file" {
		t.Fatalf("DOTENV_TEST_A = %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_B"); got != "quoted value" {
		t.Fatalf("DOTENV_TEST_B = %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_C"); got != "unquoted" {
		t.Fatalf("DOTENV_TEST_C = %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_EXISTING"); got != "from-shell" {
		t.Fatalf("existing env was overwritten: %q", got)
	}
}

func TestParseDotEnvLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
		wantErr   bool
	}{
		{name: "blank", line: "   ", wantOK: false},
		{name: "comment", line: "# hi", wantOK: false},
		{name: "simple", line: "FOO=bar", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "export", line: "export FOO=bar", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "single quotes", line: "FOO='a b'", wantKey: "FOO", wantValue: "a b", wantOK: true},
		{name: "double escapes", line: `FOO="a\nb"`, wantKey: "FOO", wantValue: "a\nb", wantOK: true},
		{name: "empty value", line: "FOO=", wantKey: "FOO", wantValue: "", wantOK: true},
		{name: "invalid", line: "not-a-pair", wantErr: true},
		{name: "unclosed quote", line: `FOO="bar`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, value, ok, err := parseDotEnvLine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDotEnvLine: %v", err)
			}
			if ok != tt.wantOK || key != tt.wantKey || value != tt.wantValue {
				t.Fatalf("got key=%q value=%q ok=%v, want key=%q value=%q ok=%v",
					key, value, ok, tt.wantKey, tt.wantValue, tt.wantOK)
			}
		})
	}
}
