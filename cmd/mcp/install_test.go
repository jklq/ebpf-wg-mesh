package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallMergesWithoutDisturbingOthers(t *testing.T) {
	root := t.TempDir()
	opts := installOptions{
		client: "claude-code", root: root,
		command: "/usr/local/bin/platform-mcp", apiURL: "https://api.example.test", token: "wgm_secret",
	}
	path, key, err := opts.configLocation()
	if err != nil {
		t.Fatal(err)
	}
	sibling := map[string]any{key: map[string]any{"other": map[string]any{"command": "x"}}, "theme": "dark"}
	data, _ := json.Marshal(sibling)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := opts.entry()
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeEntry(path, key, entry); err != nil {
		t.Fatal(err)
	}
	merged := map[string]any{}
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &merged); err != nil {
		t.Fatal(err)
	}
	servers := merged[key].(map[string]any)
	if servers["other"] == nil || merged["theme"] != "dark" {
		t.Fatalf("merge disturbed existing config: %v", merged)
	}
	ours := servers[serverName].(map[string]any)
	env := ours["env"].(map[string]any)
	if ours["command"] != "/usr/local/bin/platform-mcp" || env["PLATFORM_API_TOKEN"] != "wgm_secret" || env["PLATFORM_API_URL"] != "https://api.example.test" {
		t.Fatalf("entry = %v", ours)
	}
}

func TestInstallRemoteEntry(t *testing.T) {
	opts := installOptions{client: "vscode", root: t.TempDir(), remote: "https://mcp.example.test/mcp", token: "wgm_r"}
	path, key, err := opts.configLocation()
	if err != nil {
		t.Fatal(err)
	}
	if key != "servers" || !strings.HasSuffix(path, filepath.Join(".vscode", "mcp.json")) {
		t.Fatalf("vscode location = %s key %s", path, key)
	}
	entry, err := opts.entry()
	if err != nil {
		t.Fatal(err)
	}
	if entry["url"] != "https://mcp.example.test/mcp" {
		t.Fatalf("entry = %v", entry)
	}
	headers := entry["headers"].(map[string]string)
	if headers["Authorization"] != "Bearer wgm_r" {
		t.Fatalf("headers = %v", headers)
	}
}

func TestInstallRejectsInvalidExistingJSON(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeEntry(path, "mcpServers", map[string]any{"command": "x"}); err == nil {
		t.Fatalf("expected error for invalid existing JSON")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{oops" {
		t.Fatalf("invalid config was overwritten")
	}
}
