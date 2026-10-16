// Command mcp serves the platform MCP server.
//
// Modes:
//
//	mcp                     stdio server for local editors (default)
//	mcp --http              streamable HTTP server (remote, per-request API tokens)
//	mcp install --client X  merge an editor config entry (Railway: railway mcp install)
//	mcp config              print an editor config snippet
//
// Configuration is by flag or environment: PLATFORM_API_URL,
// PLATFORM_API_TOKEN (stdio mode), MCP_HTTP (1 enables HTTP),
// MCP_HTTP_LISTEN, MCP_PUBLIC_URL, and linked defaults
// PLATFORM_WORKSPACE_ID, PLATFORM_PROJECT_ID, PLATFORM_ENVIRONMENT_ID,
// PLATFORM_SERVICE_ID.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/apiclient"
	"ebof-wg-mesh/internal/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			if err := runInstall(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "config":
			if err := runConfig(os.Args[2:], os.Stdout); err != nil {
				fatal(err)
			}
			return
		}
	}
	if err := run(os.Args[1:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mcp:", err)
	os.Exit(1)
}

func run(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	var (
		apiURL      = fs.String("api-url", envOr("PLATFORM_API_URL", "http://127.0.0.1:9445"), "public API origin")
		apiToken    = fs.String("api-token", os.Getenv("PLATFORM_API_TOKEN"), "API token secret (stdio mode)")
		httpMode    = fs.Bool("http", os.Getenv("MCP_HTTP") == "1", "serve streamable HTTP instead of stdio")
		httpListen  = fs.String("http-listen", envOr("MCP_HTTP_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		publicURL   = fs.String("public-url", os.Getenv("MCP_PUBLIC_URL"), "public MCP endpoint URL for OAuth resource metadata")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mcp [--http] [flags] | mcp install --client <editor> | mcp config")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("ebpf-wg-mesh-mcp", mcpserver.Version)
		return nil
	}
	defaults := mcpserver.Defaults{
		WorkspaceID:   os.Getenv("PLATFORM_WORKSPACE_ID"),
		ProjectID:     os.Getenv("PLATFORM_PROJECT_ID"),
		EnvironmentID: os.Getenv("PLATFORM_ENVIRONMENT_ID"),
		ServiceID:     os.Getenv("PLATFORM_SERVICE_ID"),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *httpMode {
		return serveHTTP(ctx, *apiURL, *httpListen, *publicURL, defaults)
	}
	return serveStdio(ctx, *apiURL, *apiToken, defaults)
}

func serveStdio(ctx context.Context, apiURL, apiToken string, defaults mcpserver.Defaults) error {
	if strings.TrimSpace(apiToken) == "" {
		return errors.New("PLATFORM_API_TOKEN is required in stdio mode (create one with the console or `controlplane api-token`)")
	}
	client := apiclient.New(apiURL, apiToken)
	client.UserAgent = "ebpf-wg-mesh-mcp/" + mcpserver.Version + " (stdio)"
	server, err := mcpserver.New(client, defaults)
	if err != nil {
		return err
	}
	// All logging goes to stderr: stdout is the MCP transport.
	fmt.Fprintf(os.Stderr, "mcp: serving on stdio, api %s\n", apiURL)
	return server.Run(ctx, &mcp.StdioTransport{})
}

func serveHTTP(ctx context.Context, apiURL, listen, publicURL string, defaults mcpserver.Defaults) error {
	handler, err := mcpserver.NewHTTPHandler(mcpserver.HTTPOptions{
		APIURL: apiURL, PublicURL: publicURL, Defaults: defaults,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "mcp: serving streamable HTTP on %s (POST /mcp), api %s\n", listen, apiURL)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
