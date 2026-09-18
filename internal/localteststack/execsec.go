package localteststack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const redactedValue = "[redacted]"

// SensitiveEnvKey reports whether a variable name looks like it carries a secret.
func SensitiveEnvKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.Contains(key, "TOKEN") ||
		strings.Contains(key, "SECRET") ||
		strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "PRIVATE_KEY") ||
		strings.Contains(key, "CREDENTIAL")
}

// ScrubChildEnv merges overrides over base, dropping inherited sensitive entries.
func ScrubChildEnv(base []string, overrides map[string]string) []string {
	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, overridden := overrides[key]; overridden {
				continue
			}
			if SensitiveEnvKey(key) {
				continue
			}
		}
		merged = append(merged, entry)
	}
	for key, value := range overrides {
		merged = append(merged, key+"="+value)
	}
	return merged
}

// ChildCommand builds a child with scrubbed parent env plus explicit vars. It
// refuses values of sensitive vars in argv.
func ChildCommand(ctx context.Context, name string, args []string, env map[string]string) (*exec.Cmd, error) {
	for key, value := range env {
		if value == "" || !SensitiveEnvKey(key) {
			continue
		}
		for _, arg := range args {
			if arg != "" && strings.Contains(arg, value) {
				return nil, fmt.Errorf("refusing to place secret %s in argv of %s", key, name)
			}
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = ScrubChildEnv(os.Environ(), env)
	return cmd, nil
}

// RedactSecrets replaces each non-empty secret with "[redacted]".
func RedactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, redactedValue)
	}
	return text
}

// RedactError redacts err's message, returning err unchanged when clean.
func RedactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	redacted := RedactSecrets(err.Error(), secrets...)
	if redacted == err.Error() {
		return err
	}
	return errors.New(redacted)
}
