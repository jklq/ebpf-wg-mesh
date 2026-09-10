package localteststack

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func LoadDotEnvFile(path string) (int, error) {
	const (
		maxAttempts = 15
		retryWait   = 200 * time.Millisecond
	)

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		n, emptyFIFO, err := loadDotEnvFileOnce(path)
		if err != nil {
			return n, err
		}
		if !emptyFIFO {
			return n, nil
		}
		lastErr = fmt.Errorf("1Password-mounted .env FIFO at %s returned no content; approve the 1Password prompt if shown and retry", path)
		time.Sleep(retryWait)
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, nil
}

func loadDotEnvFileOnce(path string) (setCount int, emptyFIFO bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("stat dotenv file %s: %w", path, err)
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("open dotenv file %s: %w", path, err)
	}
	defer file.Close()

	isFIFO := info.Mode()&os.ModeNamedPipe != 0
	setCount, lines, err := applyDotEnvReader(file, path)
	if err != nil {
		return setCount, false, err
	}
	if isFIFO && lines == 0 {
		return 0, true, nil
	}
	return setCount, false, nil
}

func applyDotEnvReader(r io.Reader, path string) (setCount int, lineCount int, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok, parseErr := parseDotEnvLine(scanner.Text())
		if parseErr != nil {
			return setCount, lineNo, fmt.Errorf("parse dotenv file %s:%d: %w", path, lineNo, parseErr)
		}
		if !ok {
			continue
		}
		lineCount++
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if setErr := os.Setenv(key, value); setErr != nil {
			return setCount, lineCount, fmt.Errorf("set env %s from %s:%d: %w", key, path, lineNo, setErr)
		}
		setCount++
	}
	if err := scanner.Err(); err != nil {
		return setCount, lineCount, fmt.Errorf("read dotenv file %s: %w", path, err)
	}
	return setCount, lineCount, nil
}

func parseDotEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false, fmt.Errorf("expected KEY=VALUE")
	}
	key = strings.TrimSpace(line[:eq])
	if key == "" || strings.ContainsAny(key, " \t\"'") {
		return "", "", false, fmt.Errorf("invalid key %q", key)
	}
	raw := strings.TrimSpace(line[eq+1:])
	value, err = unquoteDotEnvValue(raw)
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

func unquoteDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, `"`) && !strings.HasPrefix(raw, `'`) {
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		return raw, nil
	}
	quote := raw[0]
	if quote != '"' && quote != '\'' {
		return raw, nil
	}
	if len(raw) == 1 || raw[len(raw)-1] != quote {
		return "", fmt.Errorf("unclosed %q quote", string(quote))
	}
	inner := raw[1 : len(raw)-1]
	if quote == '"' {
		replacer := strings.NewReplacer(
			`\n`, "\n",
			`\r`, "\r",
			`\t`, "\t",
			`\"`, `"`,
			`\\`, `\`,
		)
		return replacer.Replace(inner), nil
	}
	return inner, nil
}
