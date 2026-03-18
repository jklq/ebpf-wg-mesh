package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

type output struct {
	URL string `json:"url"`
}

func main() {
	version := flag.String("version", "v26.1.0", "cockroach version")
	flag.Parse()

	server, err := testserver.NewTestServer(
		testserver.CustomVersionOpt(*version),
	)
	if err != nil {
		log.Fatalf("start cockroach testserver: %v", err)
	}
	defer server.Stop()

	pgURL := normalizeURL(server.PGURL())
	if err := json.NewEncoder(os.Stdout).Encode(output{URL: pgURL.String()}); err != nil {
		log.Fatalf("write output: %v", err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigs:
	case <-stdinClosed():
	}
}

func normalizeURL(source *url.URL) *url.URL {
	if source == nil {
		log.Fatal("nil cockroach pg url")
	}
	clone := *source
	query := clone.Query()
	if strings.TrimSpace(query.Get("sslmode")) == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone
}

func stdinClosed() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				return
			}
		}
	}()
	return done
}
