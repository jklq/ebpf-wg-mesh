package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/testcockroach -- <command> [args...]")
		os.Exit(2)
	}

	server, err := testserver.NewTestServer(testserver.CustomVersionOpt("v26.1.0"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "start CockroachDB testserver: %v\n", err)
		os.Exit(1)
	}
	defer server.Stop()

	databaseURL, err := normalizedDatabaseURL(server.PGURL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "build CockroachDB URL: %v\n", err)
		os.Exit(1)
	}
	command := exec.Command(args[0], args[1:]...)
	command.Env = append(os.Environ(), "DASHBOARD_TEST_DATABASE_URL="+databaseURL)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "run %s: %v\n", strings.Join(args, " "), err)
		os.Exit(1)
	}
}

func normalizedDatabaseURL(source *url.URL) (string, error) {
	if source == nil {
		return "", fmt.Errorf("nil CockroachDB URL")
	}
	clone := *source
	query := clone.Query()
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return clone.String(), nil
}
