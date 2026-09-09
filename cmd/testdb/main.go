package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"ebof-wg-mesh/internal/testdb"
)

type output struct {
	URL string `json:"url"`
}

func main() {
	version := flag.String("version", testdb.DefaultVersion, "cockroach version")
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	server, err := testdb.Start(*version)
	if err != nil {
		log.Fatalf("start cockroach testserver: %v", err)
	}
	defer server.Stop()

	pgURL := testdb.NormalizeURL(server.PGURL())
	if pgURL == nil {
		log.Fatal("nil cockroach pg url")
	}

	// No command: print the database URL as JSON and serve until
	// interrupted or stdin closes.
	if len(args) == 0 {
		if err := json.NewEncoder(os.Stdout).Encode(output{URL: pgURL.String()}); err != nil {
			log.Fatalf("write output: %v", err)
		}
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sigs:
		case <-stdinClosed():
		}
		return
	}

	command := exec.Command(args[0], args[1:]...)
	command.Env = append(os.Environ(), testdb.EnvVar+"="+pgURL.String())
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
