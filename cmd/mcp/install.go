package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Editor config installation, mirroring `railway mcp install`: the entry
// is merged into the editor's existing config without disturbing other
// MCP servers.

const serverName = "ebpf-wg-mesh"

type installOptions struct {
	client  string
	remote  string // MCP HTTP endpoint URL; empty means stdio
	token   string
	apiURL  string
	command string
	root    string // project root for project-scoped configs
	home    string
}

func (o installOptions) entry() (map[string]any, error) {
	if o.remote != "" {
		entry := map[string]any{"url": o.remote}
		if o.token != "" {
			entry["headers"] = map[string]string{"Authorization": "Bearer " + o.token}
		}
		return entry, nil
	}
	if o.command == "" {
		return nil, errors.New("cannot locate this mcp binary; pass --command")
	}
	env := map[string]string{"PLATFORM_API_URL": o.apiURL}
	if o.token != "" {
		env["PLATFORM_API_TOKEN"] = o.token
	} else {
		env["PLATFORM_API_TOKEN"] = "wgm_REPLACE_ME"
	}
	return map[string]any{"command": o.command, "args": []any{}, "env": env}, nil
}

// configLocation resolves the config file and top-level servers key.
func (o installOptions) configLocation() (path, key string, err error) {
	switch o.client {
	case "claude-code":
		return filepath.Join(o.root, ".mcp.json"), "mcpServers", nil
	case "cursor":
		return filepath.Join(o.root, ".cursor", "mcp.json"), "mcpServers", nil
	case "vscode":
		return filepath.Join(o.root, ".vscode", "mcp.json"), "servers", nil
	case "windsurf":
		return filepath.Join(o.home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", nil
	case "claude-desktop":
		switch runtime.GOOS {
		case "darwin":
			return filepath.Join(o.home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), "mcpServers", nil
		case "windows":
			return filepath.Join(os.Getenv("APPDATA"), "Claude", "claude_desktop_config.json"), "mcpServers", nil
		default:
			return filepath.Join(o.home, ".config", "Claude", "claude_desktop_config.json"), "mcpServers", nil
		}
	default:
		return "", "", fmt.Errorf("unknown client %q (claude-code, claude-desktop, cursor, vscode, windsurf)", o.client)
	}
}

func mergeEntry(path, key string, entry map[string]any) error {
	config := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("existing config %s is not valid JSON: %w (left untouched)", path, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	servers, _ := config[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[serverName] = entry
	config[key] = servers
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func parseInstallFlags(args []string) (installOptions, error) {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	opts := installOptions{}
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	root, _ := os.Getwd()
	fs.StringVar(&opts.client, "client", "", "editor: claude-code, claude-desktop, cursor, vscode, windsurf")
	fs.StringVar(&opts.remote, "remote", "", "remote MCP endpoint URL (default: stdio via this binary)")
	fs.StringVar(&opts.token, "api-token", os.Getenv("PLATFORM_API_TOKEN"), "API token to embed (default: placeholder)")
	fs.StringVar(&opts.apiURL, "api-url", envOr("PLATFORM_API_URL", "http://127.0.0.1:9445"), "public API origin (stdio mode)")
	fs.StringVar(&opts.command, "command", exe, "mcp binary path for the config entry")
	fs.StringVar(&opts.root, "root", root, "project root for project-scoped configs")
	opts.home = home
	return opts, fs.Parse(args)
}

func runInstall(args []string) error {
	opts, err := parseInstallFlags(args)
	if err != nil {
		return err
	}
	if opts.client == "" {
		return errors.New("--client is required (claude-code, claude-desktop, cursor, vscode, windsurf)")
	}
	path, key, err := opts.configLocation()
	if err != nil {
		return err
	}
	entry, err := opts.entry()
	if err != nil {
		return err
	}
	if err := mergeEntry(path, key, entry); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mcp: installed %q into %s\n", serverName, path)
	if opts.remote == "" && opts.token == "" {
		fmt.Fprintln(os.Stderr, "mcp: replace PLATFORM_API_TOKEN in that file with a real API token (wgm_...), then restart the editor.")
	}
	return nil
}

// runConfig prints the config snippet for manual installation.
func runConfig(args []string, out io.Writer) error {
	opts, err := parseInstallFlags(args)
	if err != nil {
		return err
	}
	if opts.client == "" {
		opts.client = "claude-code"
	}
	_, key, err := opts.configLocation()
	if err != nil {
		return err
	}
	entry, err := opts.entry()
	if err != nil {
		return err
	}
	snippet := map[string]any{key: map[string]any{serverName: entry}}
	data, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}
